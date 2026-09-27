package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (c *Config) featureDefaults() error {
	if c.Storage.MaxPhotosPerRecord == 0 {
		c.Storage.MaxPhotosPerRecord = 3
	}
	if c.Audit.RetentionDays == 0 {
		c.Audit.RetentionDays = 180
	}
	if c.Storage.MaxPhotosPerRecord < 1 || c.Storage.MaxPhotosPerRecord > 20 {
		return fmt.Errorf("storage.max_photos_per_record 必须为 1–20")
	}
	if c.Audit.RetentionDays < 1 || c.Audit.RetentionDays > 36500 {
		return fmt.Errorf("audit.retention_days 必须为 1–36500")
	}
	return nil
}

func recordPhotos(r Record) []string {
	if len(r.Photos) > 0 {
		return r.Photos
	}
	if r.Photo != "" {
		return []string{r.Photo}
	}
	return []string{}
}

// Legacy single-photo rows remain readable without rewriting historical data.
func (r *Record) AfterFind(_ *gorm.DB) error { r.Photos = recordPhotos(*r); return nil }

func (a *App) cleanupAudit(now time.Time) error {
	cutoff := now.UTC().Add(-time.Duration(a.cfg.Audit.RetentionDays) * 24 * time.Hour)
	// julianday compares absolute times, including historical rows with local offsets.
	return a.db.Where("julianday(created_at) < julianday(?)", cutoff.Format(time.RFC3339Nano)).Delete(&Audit{}).Error
}

func (a *App) auditWorker(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			if err := a.cleanupAudit(now); err != nil {
				log.Printf("清理过期操作日志失败：%v", err)
			}
		}
	}
}

func (a *App) listAudit(c *gin.Context) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		page = 1
	}
	const size = 20
	var total int64
	rows := []Audit{}
	err = a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Audit{}).Count(&total).Error; err != nil {
			return err
		}
		pages := max(1, int((total+size-1)/size))
		page = min(page, pages)
		return tx.Order("id desc").Offset((page - 1) * size).Limit(size).Find(&rows).Error
	})
	if err != nil {
		fail(c, 500, "读取操作日志失败")
		return
	}
	c.JSON(200, gin.H{"items": rows, "total": total, "page": page, "page_size": size, "retention_days": a.cfg.Audit.RetentionDays})
}
