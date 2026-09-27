package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

func TestMultiplePhotos(t *testing.T) {
	a, c := setup(t)
	cat := category(t, c)
	keys := []string{"one.png", "two.png", "three.png", "four.png"}
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 20, 20))); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if err := a.db.Create(&Photo{Key: key, UserID: 1}).Error; err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.cfg.Storage.Uploads, key), buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	body := recordBody(cat, "in", 1)
	for _, invalid := range [][]string{keys, {"one.png", "one.png"}, {""}, {"missing.png"}} {
		body["photos"] = invalid
		expect(t, c.req("POST", "/api/records", body), 400)
	}
	a.db.Model(&Photo{}).Where("key = ?", keys[3]).Update("user_id", 999)
	body["photos"] = []string{keys[3]}
	expect(t, c.req("POST", "/api/records", body), 400)
	body["photos"] = keys[:3]
	w := c.req("POST", "/api/records", body)
	expect(t, w, 201)
	var row Record
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	var stored Record
	if err := a.db.First(&stored, row.ID).Error; err != nil || len(stored.Photos) != 3 {
		t.Fatalf("photos lost: %+v %v", stored, err)
	}
	w = c.req("GET", "/api/admin/export", nil)
	expect(t, w, 200)
	f, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, cell := range []string{"K2", "N2", "O2"} {
		images, err := f.GetPictures("出入库记录", cell)
		if err != nil || len(images) != 1 {
			t.Fatalf("%s: images=%d err=%v", cell, len(images), err)
		}
	}
	expect(t, c.req("DELETE", fmt.Sprintf("/api/admin/records/%d", row.ID), nil), 200)
	expect(t, c.req("POST", fmt.Sprintf("/api/admin/records/%d/restore", row.ID), nil), 200)
	a.cfg.Storage.MaxPhotosPerRecord = 2
	expect(t, c.req("POST", "/api/records", body), 400)
	// Simulate a legacy record without the JSON column contents.
	if err := a.db.Model(&Record{}).Where("id = ?", row.ID).Update("photos", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.db.First(&stored, row.ID).Error; err != nil || len(stored.Photos) != 1 || stored.Photos[0] != keys[0] {
		t.Fatalf("legacy photo lost: %+v %v", stored, err)
	}
}

func TestAuditPaginationAndRetention(t *testing.T) {
	a, c := setup(t)
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-180 * 24 * time.Hour)
	expired := Audit{Action: "expired", CreatedAt: cutoff.Add(-time.Second).In(time.FixedZone("UTC+8", 8*3600))}
	boundary := Audit{Action: "boundary", CreatedAt: cutoff}
	for _, row := range []*Audit{&expired, &boundary} {
		if err := a.db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := a.cleanupAudit(now); err != nil {
		t.Fatal(err)
	}
	var count int64
	a.db.Model(&Audit{}).Where("id = ?", expired.ID).Count(&count)
	if count != 0 {
		t.Fatal("expired offset timestamp not removed")
	}
	a.db.Model(&Audit{}).Where("id = ?", boundary.ID).Count(&count)
	if count != 1 {
		t.Fatal("boundary prematurely removed")
	}
	for i := 0; i < 43; i++ {
		if err := audit(a.db, 1, "test", "pagination"); err != nil {
			t.Fatal(err)
		}
	}
	var page struct {
		Items []Audit
		Total int
		Page  int
	}
	w := c.req("GET", "/api/admin/audit?page=2", nil)
	expect(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 45 || page.Page != 2 || len(page.Items) != 20 {
		t.Fatalf("invalid page: %+v", page)
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i].ID >= page.Items[i-1].ID {
			t.Fatal("unstable order")
		}
	}
	w = c.req("GET", "/api/admin/audit?page=999999999", nil)
	expect(t, w, 200)
	json.Unmarshal(w.Body.Bytes(), &page)
	if page.Page != 3 || len(page.Items) != 5 {
		t.Fatal("last page not clamped")
	}
	// Feed the production worker a deterministic tick and verify shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	ticks <- now.Add(24 * time.Hour)
	close(ticks)
	a.auditWorker(ctx, ticks)
	a.db.Model(&Audit{}).Where("id = ?", boundary.ID).Count(&count)
	if count != 0 {
		t.Fatal("worker did not clean logs")
	}
	a.cfg.Audit.RetentionDays = 1
	old := Audit{Action: "startup", CreatedAt: now.Add(-48 * time.Hour)}
	a.db.Create(&old)
	reopened, err := newApp(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db, _ := reopened.db.DB(); db.Close() }()
	reopened.db.Model(&Audit{}).Where("id = ?", old.ID).Count(&count)
	if count != 0 {
		t.Fatal("startup did not use configured retention")
	}
}

func TestFeatureConfiguration(t *testing.T) {
	var c Config
	if err := c.featureDefaults(); err != nil || c.Storage.MaxPhotosPerRecord != 3 || c.Audit.RetentionDays != 180 {
		t.Fatal("invalid defaults")
	}
	c.Storage.MaxPhotosPerRecord = 21
	if c.featureDefaults() == nil {
		t.Fatal("unbounded photo count")
	}
	c.Storage.MaxPhotosPerRecord = 5
	c.Audit.RetentionDays = -1
	if c.featureDefaults() == nil {
		t.Fatal("negative retention")
	}
	c.Audit.RetentionDays = 30
	if c.featureDefaults() != nil || c.Storage.MaxPhotosPerRecord != 5 || c.Audit.RetentionDays != 30 {
		t.Fatal("custom config lost")
	}
}
