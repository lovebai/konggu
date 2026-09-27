package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xuri/excelize/v2"
	"golang.org/x/image/draw"
)

// Embed a bounded thumbnail, rather than a full camera photo, in each Excel row.
func (a *App) exportThumbnail(key string) ([]byte, error) {
	if key == "" || filepath.Base(key) != key || strings.ContainsAny(key, "/\\") {
		return nil, fmt.Errorf("invalid photo key")
	}
	f, err := os.Open(filepath.Join(a.cfg.Storage.Uploads, key))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 32*1024*1024+1))
	if err != nil || len(data) > 32*1024*1024 {
		return nil, fmt.Errorf("invalid image size")
	}
	info, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || info.Width < 1 || info.Height < 1 || int64(info.Width)*int64(info.Height) > 40000000 {
		return nil, fmt.Errorf("invalid image")
	}
	original, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	w, h := info.Width, info.Height
	if w > 160 || h > 120 {
		scale := min(160.0/float64(w), 120.0/float64(h))
		w = max(1, int(float64(w)*scale))
		h = max(1, int(float64(h)*scale))
	}
	thumb := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(thumb, thumb.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(thumb, thumb.Bounds(), original, original.Bounds(), draw.Over, nil)
	var buf bytes.Buffer
	err = jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 80})
	return buf.Bytes(), err
}

func (a *App) export(c *gin.Context) {
	q, err := a.filtered(c)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	offset, err := strconv.Atoi(c.DefaultQuery("tz_offset", "0"))
	if err != nil || offset < -840 || offset > 840 {
		fail(c, 400, "导出时区无效")
		return
	}
	minutes := -offset
	sign := "+"
	if minutes < 0 {
		sign = "-"
		minutes = -minutes
	}
	zone := fmt.Sprintf("UTC%s%02d:%02d", sign, minutes/60, minutes%60)
	loc := time.FixedZone(zone, -offset*60)
	withImages := c.DefaultQuery("include_images", "1") != "0"
	var rows []Record
	if err := q.Preload("Product").Preload("Category").Preload("User").Order("records.occurred_at desc, records.id desc").Limit(50001).Find(&rows).Error; err != nil {
		fail(c, 500, "导出失败")
		return
	}
	if len(rows) > 50000 {
		fail(c, 400, "单次最多导出 5 万条，请缩小筛选范围")
		return
	}
	photos := 0
	for _, r := range rows {
		if r.Photo != "" {
			photos++
		}
	}
	if withImages && photos > 500 {
		fail(c, 400, "含图片导出最多 500 条带图记录，请缩小时间区间或取消包含图片")
		return
	}
	f := excelize.NewFile()
	defer f.Close()
	sheet := "出入库记录"
	f.SetSheetName("Sheet1", sheet)
	headers := []string{"记录编号", "操作时间 (" + zone + ")", "物品名称", "物品条码/编码/序列号", "分类", "方向", "数量", "单位", "登记人", "备注", "物品图片", "来源", "登记时间 (" + zone + ")"}
	if !withImages {
		headers[10] = "图片文件名"
	}
	f.SetColWidth(sheet, "A", "M", 20)
	f.SetColWidth(sheet, "B", "B", 25)
	f.SetColWidth(sheet, "D", "D", 28)
	f.SetColWidth(sheet, "J", "J", 36)
	f.SetColWidth(sheet, "K", "K", 26)
	f.SetColWidth(sheet, "M", "M", 25)
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellStr(sheet, cell, h)
	}
	cache := map[string][]byte{}
	for i, r := range rows {
		direction := "入库"
		if r.Direction == "out" {
			direction = "出库"
		}
		source := r.Source
		if source == "" {
			source = "未填写（历史记录）"
		}
		values := []string{strconv.Itoa(int(r.ID)), r.OccurredAt.In(loc).Format("2006-01-02 15:04:05"), r.Product.Name, r.Product.Barcode, r.Category.Name, direction, "", r.Product.Unit, userName(r.User), r.Note, "", source, r.CreatedAt.In(loc).Format("2006-01-02 15:04:05")}
		for j, v := range values {
			cell, _ := excelize.CoordinatesToCellName(j+1, i+2)
			f.SetCellStr(sheet, cell, v)
		}
		f.SetCellInt(sheet, fmt.Sprintf("G%d", i+2), r.Quantity)
		cell := fmt.Sprintf("K%d", i+2)
		if r.Photo == "" {
			f.SetCellStr(sheet, cell, "—")
			continue
		}
		if !withImages {
			f.SetCellStr(sheet, cell, r.Photo)
			continue
		}
		thumb, ok := cache[r.Photo]
		if !ok {
			thumb, _ = a.exportThumbnail(r.Photo)
			cache[r.Photo] = thumb
		}
		if len(thumb) == 0 {
			f.SetCellStr(sheet, cell, "图片缺失或无法读取")
			continue
		}
		f.SetRowHeight(sheet, i+2, 100)
		err := f.AddPictureFromBytes(sheet, cell, &excelize.Picture{Extension: ".jpg", File: thumb, Format: &excelize.GraphicOptions{AltText: r.Product.Name, AutoFit: true, LockAspectRatio: true, Positioning: "oneCell", OffsetX: 4, OffsetY: 4}})
		if err != nil {
			fail(c, 500, "嵌入图片失败，请取消包含图片后重试")
			return
		}
	}
	style, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Color: "FFFFFF"}, Fill: excelize.Fill{Type: "pattern", Color: []string{"147D64"}, Pattern: 1}})
	f.SetCellStyle(sheet, "A1", "M1", style)
	f.SetRowHeight(sheet, 1, 28)
	bodyStyle, _ := f.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}})
	if len(rows) > 0 {
		f.SetCellStyle(sheet, "A2", fmt.Sprintf("M%d", len(rows)+1), bodyStyle)
	}
	f.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	buf, err := f.WriteToBuffer()
	if err != nil {
		fail(c, 500, "生成文件失败")
		return
	}
	c.Header("Content-Disposition", "attachment; filename=inventory-"+time.Now().Format("20060102-150405")+".xlsx")
	c.Data(200, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", buf.Bytes())
}
