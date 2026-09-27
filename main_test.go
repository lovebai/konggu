package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xuri/excelize/v2"
)

type testClient struct {
	t       *testing.T
	handler http.Handler
	cookie  *http.Cookie
	csrf    string
}

func setup(t *testing.T) (*App, *testClient) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	var c Config
	c.Storage.Database = filepath.Join(dir, "test.db")
	c.Storage.Uploads = filepath.Join(dir, "uploads")
	c.Storage.MaxUploadMB = 1
	c.Server.SessionHours = 12
	c.Bootstrap.AdminUsername = "admin"
	c.Bootstrap.AdminPassword = "test-admin-password"
	a, err := newApp(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db, _ := a.db.DB(); db.Close() })
	client := &testClient{t: t, handler: a.router()}
	client.login("admin", "test-admin-password")
	return a, client
}
func (c *testClient) req(method, path string, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			c.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, &b)
	r.Header.Set("Content-Type", "application/json")
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	r.Header.Set("X-CSRF-Token", c.csrf)
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, r)
	return w
}
func expect(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("want %d got %d: %s", status, w.Code, w.Body.String())
	}
}
func (c *testClient) login(username, password string) {
	c.t.Helper()
	w := c.req("POST", "/api/login", gin.H{"username": username, "password": password})
	expect(c.t, w, 200)
	var d struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &d)
	c.csrf = d.CSRF
	c.cookie = w.Result().Cookies()[0]
}
func category(t *testing.T, c *testClient) uint {
	t.Helper()
	w := c.req("POST", "/api/admin/categories", gin.H{"name": "办公用品"})
	expect(t, w, 201)
	var cat Category
	json.Unmarshal(w.Body.Bytes(), &cat)
	return cat.ID
}
func recordBody(cat uint, direction string, quantity int64) gin.H {
	return gin.H{"barcode": "00123456789", "name": "A4纸", "category_id": cat, "unit": "盒", "direction": direction, "quantity": quantity, "source": "采购入库"}
}
func createRecord(t *testing.T, c *testClient, cat uint, d string, n int64) Record {
	t.Helper()
	w := c.req("POST", "/api/records", recordBody(cat, d, n))
	expect(t, w, 201)
	var r Record
	json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

func TestInventoryAndRecycle(t *testing.T) {
	a, c := setup(t)
	cat := category(t, c)
	expect(t, c.req("POST", "/api/records", recordBody(cat, "", 10)), 400)
	expect(t, c.req("POST", "/api/records", recordBody(0, "in", 10)), 400)
	expect(t, c.req("POST", "/api/records", recordBody(cat, "out", 1)), 400)
	in := createRecord(t, c, cat, "in", 10)
	out := createRecord(t, c, cat, "out", 4)
	expect(t, c.req("POST", "/api/records", recordBody(cat, "out", 7)), 400)
	n, err := balance(a.db, in.ProductID)
	if err != nil || n != 6 {
		t.Fatalf("stock %d %v", n, err)
	}
	expect(t, c.req("DELETE", fmt.Sprintf("/api/admin/records/%d", in.ID), nil), 409)
	expect(t, c.req("DELETE", fmt.Sprintf("/api/admin/records/%d", out.ID), nil), 200)
	expect(t, c.req("DELETE", fmt.Sprintf("/api/admin/records/%d", in.ID), nil), 200)
	expect(t, c.req("POST", fmt.Sprintf("/api/admin/records/%d/restore", out.ID), nil), 409)
	expect(t, c.req("POST", fmt.Sprintf("/api/admin/records/%d/restore", in.ID), nil), 200)
	expect(t, c.req("POST", fmt.Sprintf("/api/admin/records/%d/restore", out.ID), nil), 200)
	expect(t, c.req("DELETE", fmt.Sprintf("/api/admin/categories/%d", cat), nil), 409)
	expect(t, c.req("DELETE", fmt.Sprintf("/api/admin/records/%d", out.ID), nil), 200)
	expect(t, c.req("DELETE", "/api/admin/trash", nil), 200)
	n, _ = balance(a.db, in.ProductID)
	if n != 10 {
		t.Fatalf("clearing trash changed stock to %d", n)
	}
	expect(t, c.req("POST", fmt.Sprintf("/api/admin/records/%d/restore", out.ID), nil), 409)
	var count int64
	a.db.Unscoped().Model(&Record{}).Count(&count)
	if count != 1 {
		t.Fatalf("unexpected row count %d", count)
	}
}

func TestAuthPermissionsAndFilters(t *testing.T) {
	_, c := setup(t)
	cat := category(t, c)
	createRecord(t, c, cat, "in", 20)
	anon := &testClient{t: t, handler: c.handler}
	expect(t, anon.req("GET", "/api/records", nil), 401)
	original := c.csrf
	c.csrf = "invalid"
	expect(t, c.req("POST", "/api/records", recordBody(cat, "in", 1)), 403)
	c.csrf = original
	expect(t, c.req("DELETE", "/api/admin/records/1%20OR%201=1", nil), 400)
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, r)
	expect(t, w, 403)
	expect(t, c.req("POST", "/api/admin/users", gin.H{"username": "staff", "password": "staff-password-123", "role": "user"}), 201)
	u := &testClient{t: t, handler: c.handler}
	u.login("staff", "staff-password-123")
	expect(t, u.req("POST", "/api/admin/categories", gin.H{"name": "越权"}), 403)
	expect(t, u.req("GET", "/api/admin/export", nil), 403)
	own := createRecord(t, u, cat, "out", 2)
	w = u.req("GET", "/api/records", nil)
	expect(t, w, 200)
	var list struct {
		Total int `json:"total"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 1 {
		t.Fatalf("user saw others records: %s", w.Body.String())
	}
	expect(t, u.req("DELETE", fmt.Sprintf("/api/admin/records/%d", own.ID), nil), 403)
	w = c.req("GET", "/api/records?direction=out&q=A4", nil)
	expect(t, w, 200)
	json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 1 {
		t.Fatalf("filter failed: %s", w.Body.String())
	}
	expect(t, u.req("POST", "/api/password", gin.H{"old": "staff-password-123", "password": "updated-password-456"}), 200)
	expect(t, u.req("GET", "/api/me", nil), 401)
	u.login("staff", "updated-password-456")
	expect(t, c.req("PATCH", "/api/admin/users/2", gin.H{"disabled": true}), 200)
	expect(t, u.req("GET", "/api/me", nil), 401)
	expect(t, c.req("PATCH", "/api/admin/users/1", gin.H{"disabled": true}), 400)
}

func TestConcurrentOutbound(t *testing.T) {
	a, c := setup(t)
	cat := category(t, c)
	r := createRecord(t, c, cat, "in", 10)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- c.req("POST", "/api/records", recordBody(cat, "out", 7)).Code }()
	}
	wg.Wait()
	close(codes)
	successful := 0
	for code := range codes {
		if code == 201 {
			successful++
		} else if code != 400 {
			t.Fatalf("unexpected response %d", code)
		}
	}
	n, err := balance(a.db, r.ProductID)
	if successful != 1 || n != 3 || err != nil {
		t.Fatalf("concurrent stock invalid: successes=%d stock=%d err=%v", successful, n, err)
	}
}

func TestExportAndUpload(t *testing.T) {
	_, c := setup(t)
	cat := category(t, c)
	body := recordBody(cat, "in", 3)
	body["note"] = "=HYPERLINK(\"https://example.com\")"
	expect(t, c.req("POST", "/api/records", body), 201)
	w := c.req("GET", "/api/admin/export?direction=in", nil)
	expect(t, w, 200)
	f, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := f.GetRows("出入库记录")
	if err != nil || len(rows) != 2 || rows[1][3] != "00123456789" {
		t.Fatalf("export lost barcode %v %v", rows, err)
	}
	formula, _ := f.GetCellFormula("出入库记录", "J2")
	if formula != "" {
		t.Fatal("formula injection")
	}
	upload := func(data []byte) *httptest.ResponseRecorder {
		var b bytes.Buffer
		m := multipart.NewWriter(&b)
		p, _ := m.CreateFormFile("photo", "test.png")
		p.Write(data)
		m.Close()
		r := httptest.NewRequest("POST", "/api/photos", &b)
		r.Header.Set("Content-Type", m.FormDataContentType())
		r.Header.Set("X-CSRF-Token", c.csrf)
		r.AddCookie(c.cookie)
		w := httptest.NewRecorder()
		c.handler.ServeHTTP(w, r)
		return w
	}
	expect(t, upload([]byte("<svg onload=alert(1)>")), 400)
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	w = upload(b.Bytes())
	expect(t, w, 201)
	var photo struct {
		Key string `json:"key"`
		URL string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &photo)
	expect(t, c.req("GET", photo.URL, nil), 200)
	anon := &testClient{t: t, handler: c.handler}
	expect(t, anon.req("GET", photo.URL, nil), 401)
	body = recordBody(cat, "in", 1)
	body["photo"] = photo.Key
	expect(t, c.req("POST", "/api/records", body), 201)
	body["photo"] = "../../private"
	expect(t, c.req("POST", "/api/records", body), 400)
	expect(t, c.req("GET", "/", nil), 200)
	expect(t, c.req("GET", "/assets/app.js", nil), 200)
	expect(t, c.req("GET", "/healthz", nil), 200)
}

func TestLoginRateLimit(t *testing.T) {
	_, c := setup(t)
	anon := &testClient{t: t, handler: c.handler}
	for i := 0; i < 10; i++ {
		expect(t, anon.req("POST", "/api/login", gin.H{"username": "admin", "password": "bad-password"}), 401)
	}
	expect(t, anon.req("POST", "/api/login", gin.H{"username": "admin", "password": "test-admin-password"}), 429)
}

func TestProductsEmptyResultsAreArrays(t *testing.T) {
	_, c := setup(t)
	checkEmpty := func(path string) {
		t.Helper()
		w := c.req("GET", path, nil)
		expect(t, w, 200)
		if strings.TrimSpace(w.Body.String()) != "[]" {
			t.Fatalf("empty product list must be [], got %s", w.Body.String())
		}
	}
	checkEmpty("/api/products")
	cat := category(t, c)
	createRecord(t, c, cat, "in", 10)
	checkEmpty("/api/products?q=no-such-product")
	w := c.req("GET", "/api/products", nil)
	expect(t, w, 200)
	var rows []struct {
		Stock int64 `json:"stock"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Stock != 10 {
		t.Fatalf("unexpected product list: %s (%v)", w.Body.String(), err)
	}
}

func TestReverseProxyOrigins(t *testing.T) {
	a, c := setup(t)
	tests := []struct {
		name, origin, host, forwarded string
		allowed                       []string
		csrf                          bool
		status                        int
	}{
		{"original host", "https://stock.example.com", "stock.example.com", "", nil, true, 201},
		{"rewritten host", "https://stock.example.com", "127.0.0.1:8080", "", nil, true, 403},
		{"custom port", "https://stock.example.com:8443", "stock.example.com:8443", "", nil, true, 201},
		{"lost port", "https://stock.example.com:8443", "stock.example.com", "", nil, true, 403},
		{"default port and case", "https://stock.example.com", "STOCK.example.com:443", "", nil, true, 201},
		{"IPv6", "http://[::1]:8080", "[::1]:8080", "", nil, true, 201},
		{"spoofed forwarded host", "https://evil.example", "stock.example.com", "evil.example", nil, true, 403},
		{"configured proxy", "https://stock.example.com", "127.0.0.1:8080", "", []string{"https://STOCK.example.com:443/"}, true, 201},
		{"configured port", "https://stock.example.com:8443", "127.0.0.1:8080", "", []string{"https://stock.example.com:8443"}, true, 201},
		{"wrong scheme", "http://stock.example.com", "127.0.0.1:8080", "", []string{"https://stock.example.com"}, true, 403},
		{"wrong port", "https://stock.example.com:8443", "127.0.0.1:8080", "", []string{"https://stock.example.com"}, true, 403},
		{"unlisted same host", "https://other.example", "other.example", "", []string{"https://stock.example.com"}, true, 403},
		{"csrf still required", "https://stock.example.com", "127.0.0.1:8080", "", []string{"https://stock.example.com"}, false, 403},
		{"null origin", "null", "stock.example.com", "", nil, true, 403},
		{"origin with path", "https://stock.example.com/login", "stock.example.com", "", nil, true, 403},
		{"origin with credentials", "https://user@stock.example.com", "stock.example.com", "", nil, true, 403},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a.cfg.Server.AllowedOrigins = tt.allowed
			r := httptest.NewRequest("POST", "/api/admin/categories", strings.NewReader(fmt.Sprintf(`{"name":"proxy-%d"}`, i)))
			r.Host = tt.host
			r.Header.Set("Origin", tt.origin)
			r.Header.Set("X-Forwarded-Host", tt.forwarded)
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(c.cookie)
			if tt.csrf {
				r.Header.Set("X-CSRF-Token", c.csrf)
			}
			w := httptest.NewRecorder()
			c.handler.ServeHTTP(w, r)
			expect(t, w, tt.status)
			if !tt.csrf && !strings.Contains(w.Body.String(), "请求校验失败") {
				t.Fatalf("CSRF guard not reached: %s", w.Body.String())
			}
		})
	}
}

func TestAllowedOriginsConfigValidation(t *testing.T) {
	for _, tt := range []struct {
		origin string
		valid  bool
	}{
		{"https://stock.example.com", true}, {"https://stock.example.com:8443", true},
		{"*", false}, {"https://*.example.com", false}, {"stock.example.com", false},
		{"https://stock.example.com/login", false}, {"https://user@stock.example.com", false},
		{"https://stock.example.com?x=1", false}, {"https://stock.example.com#", false},
		{"https://stock.example.com:99999", false},
	} {
		t.Run(tt.origin, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := fmt.Sprintf("server:\n  address: ':8080'\n  session_hours: 12\n  allowed_origins: [%q]\nstorage:\n  database: data/test.db\n  uploads: data/uploads\n  max_upload_mb: 8\n", tt.origin)
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfig(path)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t, err=%v", tt.valid, err)
			}
		})
	}
}
