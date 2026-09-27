package main

import (
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func parseOperationTime(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.Year() < 1900 || t.Year() > 9999 {
		return time.Time{}, errors.New("invalid operation time")
	}
	return t.UTC(), nil
}

// Keep the original registration instant for old records, including trash.
// UTC normalization makes SQLite comparisons independent of the server timezone.
func migrateRecordTimes(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var records []Record
		return tx.Unscoped().Select("id", "created_at").Where("occurred_at IS NULL OR occurred_at = ?", time.Time{}).FindInBatches(&records, 200, func(_ *gorm.DB, _ int) error {
			for _, r := range records {
				if err := tx.Unscoped().Model(&Record{}).Where("id = ?", r.ID).Update("occurred_at", r.CreatedAt.UTC()).Error; err != nil {
					return err
				}
			}
			return nil
		}).Error
	})
}

func userName(u User) string {
	if name := strings.TrimSpace(u.DisplayName); name != "" {
		return name
	}
	return u.Username
}

func (a *App) profile(c *gin.Context) {
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if c.ShouldBindJSON(&in) != nil || len([]rune(strings.TrimSpace(in.DisplayName))) > 50 {
		fail(c, 400, "姓名最多 50 个字符")
		return
	}
	u := current(c)
	u.DisplayName = strings.TrimSpace(in.DisplayName)
	err := a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&u).Update("display_name", u.DisplayName).Error; err != nil {
			return err
		}
		return audit(tx, u.ID, "user.profile", "更新个人姓名")
	})
	if err != nil {
		fail(c, 500, "保存姓名失败")
		return
	}
	c.JSON(200, u)
}
