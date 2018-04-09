// TODO: more expire/cleanup tests?

package xormstore

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lafriks/xormstore/util"

	_ "github.com/denisenkom/go-mssqldb"
	_ "github.com/go-sql-driver/mysql"
	"github.com/go-xorm/core"
	"github.com/go-xorm/xorm"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

// default test db
var dbURI = "sqlite3://file:dummy?mode=memory&cache=shared"

// TODO: this is ugly
func parseCookies(value string) map[string]*http.Cookie {
	m := map[string]*http.Cookie{}
	for _, c := range (&http.Request{Header: http.Header{"Cookie": {value}}}).Cookies() {
		m[c.Name] = c
	}
	return m
}

func connectDbURI(uri string) (*xorm.Engine, error) {
	parts := strings.SplitN(uri, "://", 2)
	driver := parts[0]
	dsn := parts[1]

	var err error
	// retry to give some time for db to be ready
	for i := 0; i < 300; i++ {
		e, err := xorm.NewEngine(driver, dsn)
		e.SetLogLevel(core.LOG_WARNING)
		if err == nil {
			if err := e.Ping(); err == nil {
				return e, nil
			}
		}
		time.Sleep(time.Second)
	}

	return nil, err
}

// create new shared in memory db
func newEngine() *xorm.Engine {
	var err error
	var e *xorm.Engine
	if e, err = connectDbURI(dbURI); err != nil {
		panic(err)
	}

	//e.ShowSQL(true)

	// cleanup db
	if err := e.DropTables(
		&xormSession{tableName: "abc"},
		&xormSession{tableName: "sessions"},
	); err != nil {
		panic(err)
	}

	return e
}

func req(handler http.HandlerFunc, sessionCookie *http.Cookie) *httptest.ResponseRecorder {
	req, _ := http.NewRequest("GET", "http://test", nil)
	if sessionCookie != nil {
		req.Header.Add("Cookie", fmt.Sprintf("%s=%s", sessionCookie.Name, sessionCookie.Value))
	}
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func match(t *testing.T, resp *httptest.ResponseRecorder, code int, body string) {
	if resp.Code != code {
		t.Errorf("Expected %v, actual %v", code, resp.Code)
	}
	// http.Error in countHandler adds a \n
	if strings.Trim(resp.Body.String(), "\n") != body {
		t.Errorf("Expected %v, actual %v", body, resp.Body)
	}
}

func findSession(e *xorm.Engine, store *Store, id string) *xormSession {
	s := &xormSession{tableName: store.opts.TableName}
	if has, err := e.ID(id).Get(s); !has || err != nil {
		return nil
	}
	return s
}

func makeCountHandler(name string, store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := store.Get(r, name)
		if err != nil {
			panic(err)
		}

		count, _ := session.Values["count"].(int)
		count++
		session.Values["count"] = count
		if err := store.Save(r, w, session); err != nil {
			panic(err)
		}
		// leak session ID so we can mess with it in the db
		w.Header().Add("X-Session", session.ID)
		http.Error(w, fmt.Sprintf("%d", count), http.StatusOK)
	}
}

func TestBasic(t *testing.T) {
	store, err := New(newEngine(), []byte("secret"))
	if err != nil {
		t.Errorf("New init failed: %d", err)
		return
	}
	countFn := makeCountHandler("session", store)
	r1 := req(countFn, nil)
	match(t, r1, 200, "1")
	r2 := req(countFn, parseCookies(r1.Header().Get("Set-Cookie"))["session"])
	match(t, r2, 200, "2")
}

func TestExpire(t *testing.T) {
	e := newEngine()
	store, err := New(e, []byte("secret"))
	if err != nil {
		t.Errorf("New init failed: %d", err)
		return
	}
	countFn := makeCountHandler("session", store)

	r1 := req(countFn, nil)
	match(t, r1, 200, "1")

	// test still in db but expired
	id := r1.Header().Get("X-Session")
	s := findSession(e, store, id)
	s.ExpiresUnix = util.TimeStampNow().AddDuration(-40 * 24 * time.Hour)
	e.ID(s.ID).Cols("expires_unix").Update(s)

	r2 := req(countFn, parseCookies(r1.Header().Get("Set-Cookie"))["session"])
	match(t, r2, 200, "1")

	store.Cleanup()

	if findSession(e, store, id) != nil {
		t.Error("Expected session to be deleted")
	}
}

func TestBrokenCookie(t *testing.T) {
	store, err := New(newEngine(), []byte("secret"))
	if err != nil {
		t.Errorf("New init failed: %d", err)
		return
	}
	countFn := makeCountHandler("session", store)

	r1 := req(countFn, nil)
	match(t, r1, 200, "1")

	cookie := parseCookies(r1.Header().Get("Set-Cookie"))["session"]
	cookie.Value += "junk"
	r2 := req(countFn, cookie)
	match(t, r2, 200, "1")
}

func TestMaxAgeNegative(t *testing.T) {
	e := newEngine()
	store, err := New(e, []byte("secret"))
	if err != nil {
		t.Errorf("New init failed: %d", err)
		return
	}
	countFn := makeCountHandler("session", store)

	r1 := req(countFn, nil)
	match(t, r1, 200, "1")

	r2 := req(func(w http.ResponseWriter, r *http.Request) {
		session, err := store.Get(r, "session")
		if err != nil {
			panic(err)
		}

		session.Options.MaxAge = -1
		store.Save(r, w, session)

		http.Error(w, "", http.StatusOK)
	}, parseCookies(r1.Header().Get("Set-Cookie"))["session"])

	match(t, r2, 200, "")
	c := parseCookies(r2.Header().Get("Set-Cookie"))["session"]
	if c != nil && c.Value != "" {
		t.Error("Expected empty Set-Cookie session header", c)
	}

	id := r1.Header().Get("X-Session")
	if s := findSession(e, store, id); s != nil {
		t.Error("Expected session to be deleted")
	}
}

func TestMaxLength(t *testing.T) {
	store, err := New(newEngine(), []byte("secret"))
	if err != nil {
		t.Errorf("New init failed: %d", err)
		return
	}
	store.MaxLength(10)

	r1 := req(func(w http.ResponseWriter, r *http.Request) {
		session, err := store.Get(r, "session")
		if err != nil {
			panic(err)
		}

		session.Values["a"] = "aaaaaaaaaaaaaaaaaaaaaaaa"
		if err := store.Save(r, w, session); err == nil {
			t.Error("Expected too large error")
		}

		http.Error(w, "", http.StatusOK)
	}, nil)
	match(t, r1, 200, "")
}

func TestTableName(t *testing.T) {
	e := newEngine()
	store, err := NewOptions(e, Options{TableName: "abc"}, []byte("secret"))
	if err != nil {
		t.Errorf("NewOptions init failed: %d", err)
		return
	}
	countFn := makeCountHandler("session", store)

	if has, err := e.IsTableExist(&xormSession{tableName: store.opts.TableName}); !has || err != nil {
		t.Error("Expected abc table created")
	}

	r1 := req(countFn, nil)
	match(t, r1, 200, "1")
	r2 := req(countFn, parseCookies(r1.Header().Get("Set-Cookie"))["session"])
	match(t, r2, 200, "2")

	id := r2.Header().Get("X-Session")
	s := findSession(e, store, id)
	s.ExpiresUnix = util.TimeStampNow().AddDuration(-40 * 24 * time.Hour)
	e.ID(s.ID).Cols("expires_unix").Update(s)

	store.Cleanup()

	if findSession(e, store, id) != nil {
		t.Error("Expected session to be deleted")
	}
}

func TestSkipCreateTable(t *testing.T) {
	e := newEngine()
	store, err := NewOptions(e, Options{SkipCreateTable: true}, []byte("secret"))
	if err != nil {
		t.Errorf("NewOptions init failed: %d", err)
		return
	}

	if has, err := e.IsTableExist(&xormSession{tableName: store.opts.TableName}); has || err != nil {
		t.Error("Expected no table created")
	}
}

func TestMultiSessions(t *testing.T) {
	store, err := New(newEngine(), []byte("secret"))
	if err != nil {
		t.Errorf("New init failed: %d", err)
		return
	}
	countFn1 := makeCountHandler("session1", store)
	countFn2 := makeCountHandler("session2", store)

	r1 := req(countFn1, nil)
	match(t, r1, 200, "1")
	r2 := req(countFn2, nil)
	match(t, r2, 200, "1")

	r3 := req(countFn1, parseCookies(r1.Header().Get("Set-Cookie"))["session1"])
	match(t, r3, 200, "2")
	r4 := req(countFn2, parseCookies(r2.Header().Get("Set-Cookie"))["session2"])
	match(t, r4, 200, "2")
}

func TestPeriodicCleanup(t *testing.T) {
	e := newEngine()
	store, err := New(e, []byte("secret"))
	if err != nil {
		t.Errorf("New init failed: %d", err)
		return
	}
	store.SessionOpts.MaxAge = 1
	countFn := makeCountHandler("session", store)

	quit := make(chan struct{})
	go store.PeriodicCleanup(200*time.Millisecond, quit)

	// test that cleanup i done at least twice

	r1 := req(countFn, nil)
	id1 := r1.Header().Get("X-Session")

	if findSession(e, store, id1) == nil {
		t.Error("Expected r1 session to exist")
	}

	time.Sleep(2 * time.Second)

	if findSession(e, store, id1) != nil {
		t.Error("Expected r1 session to be deleted")
	}

	r2 := req(countFn, nil)
	id2 := r2.Header().Get("X-Session")

	if findSession(e, store, id2) == nil {
		t.Error("Expected r2 session to exist")
	}

	time.Sleep(2 * time.Second)

	if findSession(e, store, id2) != nil {
		t.Error("Expected r2 session to be deleted")
	}

	close(quit)

	// test that cleanup has stopped

	r3 := req(countFn, nil)
	id3 := r3.Header().Get("X-Session")

	if findSession(e, store, id3) == nil {
		t.Error("Expected r3 session to exist")
	}

	time.Sleep(2 * time.Second)

	if findSession(e, store, id3) == nil {
		t.Error("Expected r3 session to exist")
	}
}

func TestMain(m *testing.M) {
	flag.Parse()

	if v := os.Getenv("DATABASE_URI"); v != "" {
		dbURI = v
	}
	fmt.Printf("DATABASE_URI=%s\n", dbURI)

	os.Exit(m.Run())
}
