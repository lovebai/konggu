package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xuri/excelize/v2"
)

func TestRecordSourceTimeAndRange(t *testing.T) {
	_, c := setup(t)
	cat := category(t, c)
	body := recordBody(cat, "in", 10)
	delete(body, "source")
	expect(t, c.req("POST", "/api/records", body), 400)
	body["source"] = "  "
	expect(t, c.req("POST", "/api/records", body), 400)
	body["source"] = strings.Repeat("源", 101)
	expect(t, c.req("POST", "/api/records", body), 400)
	body["source"] = "  总部采购  "
	body["occurred_at"] = "bad-time"
	expect(t, c.req("POST", "/api/records", body), 400)
	body["occurred_at"] = "2026-01-15T08:30:00+08:00"
	w := c.req("POST", "/api/records", body)
	expect(t, w, 201)
	var r Record
	json.Unmarshal(w.Body.Bytes(), &r)
	if r.Source != "总部采购" || r.OccurredAt.UTC().Format(time.RFC3339) != "2026-01-15T00:30:00Z" {
		t.Fatalf("invalid fields: %+v", r)
	}
	now := createRecord(t, c, cat, "in", 2)
	if time.Since(now.OccurredAt) > time.Minute || now.OccurredAt.IsZero() {
		t.Fatal("default time missing")
	}
	p := url.Values{"start": {"2026-01-15T08:30:00+08:00"}, "end": {"2026-01-15T08:30:00+08:00"}}
	w = c.req("GET", "/api/records?"+p.Encode(), nil)
	expect(t, w, 200)
	var list struct {
		Total int64    `json:"total"`
		Items []Record `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].ID != r.ID {
		t.Fatalf("range boundary failed: %s", w.Body.String())
	}
	w = c.req("GET", "/api/admin/export?"+p.Encode()+"&tz_offset=-480", nil)
	expect(t, w, 200)
	f, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, _ := f.GetRows("出入库记录")
	if len(rows) != 2 || rows[1][1] != "2026-01-15 08:30:00" || rows[1][11] != "总部采购" {
		t.Fatalf("export range/source/time invalid: %v", rows)
	}
	for _, path := range []string{"/api/records", "/api/admin/export"} {
		expect(t, c.req("GET", path+"?start=not-a-date", nil), 400)
		expect(t, c.req("GET", path+"?start=2026-02-01T00:00:00Z&end=2026-01-01T00:00:00Z", nil), 400)
	}
	p.Set("start", "2000-01-01T00:00:00Z")
	p.Set("end", "2000-01-02T00:00:00Z")
	w = c.req("GET", "/api/admin/export?"+p.Encode(), nil)
	expect(t, w, 200)
	empty, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	emptyRows, _ := empty.GetRows("出入库记录")
	if len(emptyRows) != 1 {
		t.Fatal("empty range should export only headers")
	}
}

func TestDisplayNamesAndPermissions(t *testing.T) {
	a, c := setup(t)
	expect(t, c.req("POST", "/api/admin/users", gin.H{"username": "named", "password": "named-password-123", "role": "user", "display_name": "  张三  "}), 201)
	user := &testClient{t: t, handler: c.handler}
	user.login("named", "named-password-123")
	w := user.req("GET", "/api/me", nil)
	expect(t, w, 200)
	if !strings.Contains(w.Body.String(), "张三") {
		t.Fatal("name missing")
	}
	expect(t, user.req("PATCH", "/api/profile", gin.H{"display_name": "李四", "role": "admin"}), 200)
	var u User
	a.db.First(&u, 2)
	if u.Role != "user" || userName(u) != "李四" {
		t.Fatalf("profile update invalid %+v", u)
	}
	cat := category(t, c)
	createRecord(t, user, cat, "in", 1)
	w = c.req("GET", "/api/admin/export", nil)
	expect(t, w, 200)
	f, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	name, _ := f.GetCellValue("出入库记录", "I2")
	if name != "李四" {
		t.Fatalf("export name=%s", name)
	}
	expect(t, user.req("PATCH", "/api/admin/users/1", gin.H{"display_name": "管理员"}), 403)
	expect(t, c.req("PATCH", "/api/admin/users/2", gin.H{"disabled": true}), 200)
	expect(t, c.req("PATCH", "/api/admin/users/2", gin.H{"display_name": "王五"}), 200)
	a.db.First(&u, 2)
	if !u.Disabled {
		t.Fatal("name edit must not re-enable account")
	}
	expect(t, c.req("PATCH", "/api/admin/users/1", gin.H{"display_name": "负责人"}), 200)
	expect(t, c.req("GET", "/api/me", nil), 200)
	expect(t, c.req("PATCH", "/api/profile", gin.H{"display_name": " "}), 200)
	var adminUser User
	a.db.First(&adminUser, 1)
	if userName(adminUser) != "admin" {
		t.Fatal("empty name must fall back to username")
	}
	expect(t, c.req("PATCH", "/api/profile", gin.H{"display_name": strings.Repeat("名", 51)}), 400)
}

func TestExportEmbeddedImages(t *testing.T) {
	a, c := setup(t)
	cat := category(t, c)
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 320, 240)))
	key := "test-photo.png"
	if err := os.WriteFile(filepath.Join(a.cfg.Storage.Uploads, key), buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Create(&Photo{Key: key, UserID: 1}).Error; err != nil {
		t.Fatal(err)
	}
	body := recordBody(cat, "in", 5)
	body["photo"] = key
	expect(t, c.req("POST", "/api/records", body), 201)
	w := c.req("GET", "/api/admin/export", nil)
	expect(t, w, 200)
	f, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pictures, err := f.GetPictures("出入库记录", "K2")
	if err != nil || len(pictures) != 1 {
		t.Fatalf("no embedded image: %d %v", len(pictures), err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(pictures[0].File))
	if err != nil || cfg.Width > 160 || cfg.Height > 120 {
		t.Fatalf("unbounded thumbnail: %+v %v", cfg, err)
	}
	if err := f.SetCellFormula("出入库记录", "N2", "SUM(G2)"); err != nil {
		t.Fatal(err)
	}
	sum, err := f.CalcCellValue("出入库记录", "N2")
	if err != nil || sum != "5" {
		t.Fatalf("quantity cannot be summed: %s %v", sum, err)
	}
	w = c.req("GET", "/api/admin/export?include_images=0", nil)
	expect(t, w, 200)
	plain, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	pictures, _ = plain.GetPictures("出入库记录", "K2")
	value, _ := plain.GetCellValue("出入库记录", "K2")
	if len(pictures) != 0 || value != key {
		t.Fatal("text-only export incorrect")
	}
	if err := os.Remove(filepath.Join(a.cfg.Storage.Uploads, key)); err != nil {
		t.Fatal(err)
	}
	w = c.req("GET", "/api/admin/export", nil)
	expect(t, w, 200)
	missing, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Close()
	value, _ = missing.GetCellValue("出入库记录", "K2")
	if !strings.Contains(value, "图片缺失") {
		t.Fatal("missing image must be visible")
	}
}

func TestLegacyOperationTimeMigration(t *testing.T) {
	a, c := setup(t)
	cat := category(t, c)
	r := createRecord(t, c, cat, "in", 1)
	trash := createRecord(t, c, cat, "in", 1)
	historical := time.Date(2021, 5, 6, 12, 34, 56, 0, time.FixedZone("CST", 8*3600))
	if err := a.db.Unscoped().Model(&Record{}).Where("id IN ?", []uint{r.ID, trash.ID}).Updates(map[string]any{"occurred_at": nil, "created_at": historical, "source": ""}).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.db.Delete(&Record{}, trash.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateRecordTimes(a.db); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint{r.ID, trash.ID} {
		var row Record
		a.db.Unscoped().First(&row, id)
		if !row.OccurredAt.Equal(historical) || row.Source != "" {
			t.Fatalf("legacy values lost: %+v", row)
		}
	}
	if err := migrateRecordTimes(a.db); err != nil {
		t.Fatal(err)
	}
	w := c.req("GET", "/api/records?start=2021-05-06T04:34:56Z&end=2021-05-06T04:34:56Z", nil)
	expect(t, w, 200)
	if !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatal(w.Body.String())
	}
	// Simulate reopening an old database without the new user/record columns.
	if err := a.db.Migrator().DropIndex(&Record{}, "idx_records_occurred_at"); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"occurred_at", "source"} {
		if err := a.db.Exec("ALTER TABLE records DROP COLUMN " + column).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := a.db.Exec("ALTER TABLE users DROP COLUMN display_name").Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := a.db.DB()
	sqlDB.Close()
	upgraded, err := newApp(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { sqlDB, _ := upgraded.db.DB(); sqlDB.Close() }()
	var row Record
	if err := upgraded.db.First(&row, r.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !row.OccurredAt.Equal(historical) {
		t.Fatal("startup migration did not preserve time")
	}
	var count int64
	upgraded.db.Unscoped().Model(&Record{}).Count(&count)
	if count != 2 {
		t.Fatal("migration lost records")
	}
}
