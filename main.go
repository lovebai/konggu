package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

//go:embed web/*
var frontend embed.FS

type Config struct {
	Server struct {
		Address        string   `yaml:"address"`
		SecureCookie   bool     `yaml:"secure_cookie"`
		SessionHours   int      `yaml:"session_hours"`
		AllowedOrigins []string `yaml:"allowed_origins"`
	} `yaml:"server"`
	Storage struct {
		Database    string `yaml:"database"`
		Uploads     string `yaml:"uploads"`
		MaxUploadMB int64  `yaml:"max_upload_mb"`
	} `yaml:"storage"`
	Bootstrap struct {
		AdminUsername string `yaml:"admin_username"`
		AdminPassword string `yaml:"admin_password"`
	} `yaml:"bootstrap"`
}

type User struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	Username    string    `json:"username" gorm:"uniqueIndex;size:64"`
	DisplayName string    `json:"display_name" gorm:"size:100"`
	Password    string    `json:"-"`
	Role        string    `json:"role"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
}
type Session struct {
	Hash      string `gorm:"primaryKey"`
	UserID    uint   `gorm:"index"`
	CSRF      string
	ExpiresAt time.Time `gorm:"index"`
}
type Category struct {
	ID   uint   `json:"id" gorm:"primaryKey"`
	Name string `json:"name" gorm:"uniqueIndex;size:64"`
}
type Product struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	Barcode    string `json:"barcode" gorm:"uniqueIndex;size:128"`
	Name       string `json:"name"`
	CategoryID uint   `json:"category_id" gorm:"index"`
	Unit       string `json:"unit"`
}
type Record struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	ProductID  uint           `json:"product_id" gorm:"index"`
	Product    Product        `json:"product"`
	CategoryID uint           `json:"category_id" gorm:"index"`
	Category   Category       `json:"category"`
	UserID     uint           `json:"user_id" gorm:"index"`
	User       User           `json:"user"`
	Direction  string         `json:"direction" gorm:"index"`
	Quantity   int64          `json:"quantity"`
	Photo      string         `json:"photo"`
	Note       string         `json:"note"`
	Source     string         `json:"source" gorm:"size:200"`
	OccurredAt time.Time      `json:"occurred_at" gorm:"index"`
	CreatedAt  time.Time      `json:"created_at" gorm:"index"`
	DeletedAt  gorm.DeletedAt `json:"deleted_at" gorm:"index"`
}
type Photo struct {
	Key       string `gorm:"primaryKey"`
	UserID    uint
	CreatedAt time.Time
}
type Audit struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	UserID    uint      `json:"user_id"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}
type attempt struct {
	Count int
	Until time.Time
}
type App struct {
	db       *gorm.DB
	cfg      Config
	mu       sync.Mutex
	attempts map[string]attempt
}

func loadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if v := os.Getenv("INVENTORY_ADMIN_PASSWORD"); v != "" {
		c.Bootstrap.AdminPassword = v
	}
	if c.Server.Address == "" || c.Storage.Database == "" || c.Storage.Uploads == "" || c.Server.SessionHours < 1 || c.Server.SessionHours > 168 || c.Storage.MaxUploadMB < 1 || c.Storage.MaxUploadMB > 32 {
		return c, errors.New("config.yaml 配置无效：检查地址、存储路径、会话时长和上传上限")
	}
	for _, origin := range c.Server.AllowedOrigins {
		if _, err := canonicalOrigin(origin); err != nil {
			return c, fmt.Errorf("server.allowed_origins 中的地址 %q 无效：仅允许 http(s)://域名[:端口]", origin)
		}
	}
	return c, nil
}

// Normalize exact origins, including default ports; never trust forwarded headers
// to widen the configured allowlist.
func canonicalOrigin(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || strings.ContainsAny(origin, "*#") || strings.HasSuffix(u.Host, ":") {
		return "", errors.New("invalid origin")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("invalid port")
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(n)), nil
}

func (a *App) originAllowed(origin, host string) bool {
	value, err := canonicalOrigin(origin)
	if err != nil {
		return false
	}
	if len(a.cfg.Server.AllowedOrigins) > 0 {
		for _, configured := range a.cfg.Server.AllowedOrigins {
			allowed, err := canonicalOrigin(configured)
			if err == nil && allowed == value {
				return true
			}
		}
		return false
	}
	// With no explicit list, preserve the original same-host policy for proxies
	// that terminate TLS and forward the original Host to an HTTP backend.
	u, _ := url.Parse(origin)
	expected, err := canonicalOrigin(u.Scheme + "://" + host)
	return err == nil && value == expected
}

func newApp(c Config) (*App, error) {
	if err := os.MkdirAll(filepath.Dir(c.Storage.Database), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.Storage.Uploads, 0700); err != nil {
		return nil, err
	}
	db, err := gorm.Open(sqlite.Open(c.Storage.Database), &gorm.Config{TranslateError: true})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if err = db.Exec(q).Error; err != nil {
			return nil, err
		}
	}
	if err = db.AutoMigrate(&User{}, &Session{}, &Category{}, &Product{}, &Record{}, &Photo{}, &Audit{}); err != nil {
		return nil, err
	}
	if err = migrateRecordTimes(db); err != nil {
		return nil, err
	}
	var n int64
	db.Model(&User{}).Count(&n)
	if n == 0 {
		if len(c.Bootstrap.AdminPassword) < 12 || len(c.Bootstrap.AdminPassword) > 72 || strings.TrimSpace(c.Bootstrap.AdminUsername) == "" {
			return nil, errors.New("首次启动请设置 INVENTORY_ADMIN_PASSWORD（12–72 字节），禁止使用默认密码")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(c.Bootstrap.AdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		if err = db.Create(&User{Username: c.Bootstrap.AdminUsername, Password: string(hash), Role: "admin"}).Error; err != nil {
			return nil, err
		}
	}
	return &App{db: db, cfg: c, attempts: map[string]attempt{}}, nil
}

func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func digest(s string) string                      { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func fail(c *gin.Context, status int, msg string) { c.AbortWithStatusJSON(status, gin.H{"error": msg}) }
func current(c *gin.Context) User                 { return c.MustGet("user").(User) }
func admin(c *gin.Context) {
	if current(c).Role != "admin" {
		fail(c, 403, "需要管理员权限")
		return
	}
	c.Next()
}
func audit(tx *gorm.DB, uid uint, action, detail string) error {
	return tx.Create(&Audit{UserID: uid, Action: action, Detail: detail}).Error
}
func (a *App) auth(c *gin.Context) {
	if id := c.Param("id"); id != "" {
		if n, err := strconv.ParseUint(id, 10, 32); err != nil || n == 0 {
			fail(c, 400, "无效编号")
			return
		}
	}
	s, err := c.Cookie("inventory_session")
	if err != nil {
		fail(c, 401, "请先登录")
		return
	}
	var session Session
	if a.db.Where("hash = ? AND expires_at > ?", digest(s), time.Now()).First(&session).Error != nil {
		fail(c, 401, "登录已过期，请重新登录")
		return
	}
	var u User
	if a.db.First(&u, session.UserID).Error != nil || u.Disabled {
		fail(c, 401, "账号不可用")
		return
	}
	if c.Request.Method != "GET" && c.Request.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(c.GetHeader("X-CSRF-Token")), []byte(session.CSRF)) != 1 {
		fail(c, 403, "请求校验失败，请刷新页面重试")
		return
	}
	c.Set("user", u)
	c.Set("csrf", session.CSRF)
	c.Set("session", session.Hash)
	c.Next()
}

func (a *App) router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	_ = r.SetTrustedProxies(nil)
	r.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "same-origin")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; connect-src 'self'; media-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		c.Header("Permissions-Policy", "camera=(self)")
		if strings.HasPrefix(c.Request.URL.Path, "/api/") || strings.HasPrefix(c.Request.URL.Path, "/uploads/") {
			c.Header("Cache-Control", "no-store")
		}
		if c.Request.Method != "GET" && c.Request.Method != "HEAD" {
			if origin := c.GetHeader("Origin"); origin != "" {
				if !a.originAllowed(origin, c.Request.Host) {
					fail(c, 403, "不允许跨站请求")
					return
				}
			}
			limit := int64(64 * 1024)
			if c.Request.URL.Path == "/api/photos" {
				limit = a.cfg.Storage.MaxUploadMB*1024*1024 + 1024*1024
			}
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	})
	r.GET("/healthz", func(c *gin.Context) {
		sqlDB, _ := a.db.DB()
		if err := sqlDB.PingContext(c); err != nil {
			fail(c, 503, "数据库不可用")
			return
		}
		c.JSON(200, gin.H{"status": "ok"})
	})
	r.POST("/api/login", a.login)
	api := r.Group("/api", a.auth)
	api.GET("/me", func(c *gin.Context) {
		c.JSON(200, gin.H{"user": current(c), "csrf": c.GetString("csrf"), "max_upload_mb": a.cfg.Storage.MaxUploadMB})
	})
	api.POST("/logout", func(c *gin.Context) {
		a.db.Delete(&Session{}, "hash = ?", c.GetString("session"))
		c.SetCookie("inventory_session", "", -1, "/", "", a.cfg.Server.SecureCookie, true)
		c.JSON(200, gin.H{"ok": true})
	})
	api.POST("/password", a.password)
	api.PATCH("/profile", a.profile)
	api.GET("/categories", func(c *gin.Context) {
		var rows []Category
		if a.db.Order("name").Find(&rows).Error != nil {
			fail(c, 500, "读取分类失败")
			return
		}
		c.JSON(200, rows)
	})
	api.GET("/products", a.products)
	api.POST("/photos", a.upload)
	api.POST("/records", a.createRecord)
	api.GET("/records", a.records)
	api.GET("/stats", a.stats)
	management := api.Group("/admin", admin)
	management.POST("/categories", a.createCategory)
	management.DELETE("/categories/:id", a.deleteCategory)
	management.GET("/export", a.export)
	management.DELETE("/records/:id", a.trash)
	management.POST("/records/:id/restore", a.restore)
	management.DELETE("/trash", a.emptyTrash)
	management.GET("/users", func(c *gin.Context) { var users []User; a.db.Order("id").Find(&users); c.JSON(200, users) })
	management.POST("/users", a.createUser)
	management.PATCH("/users/:id", a.updateUser)
	management.GET("/audit", func(c *gin.Context) {
		var rows []Audit
		a.db.Order("id desc").Limit(200).Find(&rows)
		c.JSON(200, rows)
	})
	r.GET("/uploads/:key", a.auth, func(c *gin.Context) {
		key := c.Param("key")
		if filepath.Base(key) != key || strings.ContainsAny(key, "/\\") {
			fail(c, 404, "图片不存在")
			return
		}
		var p Photo
		if a.db.First(&p, "key = ?", key).Error != nil {
			fail(c, 404, "图片不存在")
			return
		}
		c.File(filepath.Join(a.cfg.Storage.Uploads, key))
	})
	assets, _ := fs.Sub(frontend, "web")
	r.GET("/", func(c *gin.Context) {
		b, _ := fs.ReadFile(assets, "index.html")
		c.Data(200, "text/html; charset=utf-8", b)
	})
	r.GET("/assets/:file", func(c *gin.Context) {
		name := c.Param("file")
		if strings.ContainsAny(name, "/\\") {
			c.Status(404)
			return
		}
		http.FileServer(http.FS(assets)).ServeHTTP(c.Writer, clonePath(c.Request, "/"+name))
	})
	return r
}
func clonePath(r *http.Request, p string) *http.Request {
	v := r.Clone(r.Context())
	u := *r.URL
	u.Path = p
	v.URL = &u
	return v
}

func (a *App) login(c *gin.Context) {
	ip := c.ClientIP()
	now := time.Now()
	a.mu.Lock()
	for k, v := range a.attempts {
		if now.After(v.Until) {
			delete(a.attempts, k)
		}
	}
	v := a.attempts[ip]
	if v.Count >= 10 {
		a.mu.Unlock()
		fail(c, 429, "登录尝试过于频繁，请 15 分钟后重试")
		return
	}
	if v.Count == 0 {
		v.Until = now.Add(15 * time.Minute)
	}
	v.Count++
	a.attempts[ip] = v
	a.mu.Unlock()
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if c.ShouldBindJSON(&in) != nil || len(in.Password) > 72 {
		fail(c, 400, "请输入有效账号密码")
		return
	}
	var u User
	err := a.db.Where("username = ?", strings.TrimSpace(in.Username)).First(&u).Error
	hash := u.Password
	if err != nil {
		hash = "$2a$10$7EqJtq98hPqEX7fNZaFWoO5J0ZPS2hKcWTFOqOEU5KGEPmoRMw1vK"
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil || err != nil || u.Disabled {
		fail(c, 401, "账号或密码错误")
		return
	}
	s, csrf := token(), token()
	err = a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at < ?", now).Delete(&Session{}).Error; err != nil {
			return err
		}
		if err := tx.Create(&Session{Hash: digest(s), UserID: u.ID, CSRF: csrf, ExpiresAt: now.Add(time.Duration(a.cfg.Server.SessionHours) * time.Hour)}).Error; err != nil {
			return err
		}
		return audit(tx, u.ID, "login", "登录")
	})
	if err != nil {
		fail(c, 500, "登录失败")
		return
	}
	a.mu.Lock()
	delete(a.attempts, ip)
	a.mu.Unlock()
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("inventory_session", s, a.cfg.Server.SessionHours*3600, "/", "", a.cfg.Server.SecureCookie, true)
	c.JSON(200, gin.H{"user": u, "csrf": csrf, "max_upload_mb": a.cfg.Storage.MaxUploadMB})
}
func (a *App) password(c *gin.Context) {
	var in struct {
		Old      string `json:"old"`
		Password string `json:"password"`
	}
	u := current(c)
	if c.ShouldBindJSON(&in) != nil || len(in.Password) < 12 || len(in.Password) > 72 || bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(in.Old)) != nil {
		fail(c, 400, "原密码错误或新密码不符合 12–72 字节要求")
		return
	}
	h, _ := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	err := a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&u).Update("password", string(h)).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", u.ID).Delete(&Session{}).Error; err != nil {
			return err
		}
		return audit(tx, u.ID, "password", "修改密码并注销所有会话")
	})
	if err != nil {
		fail(c, 500, "修改失败")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func balance(tx *gorm.DB, pid uint) (int64, error) {
	var n int64
	err := tx.Model(&Record{}).Where("product_id = ?", pid).Select("COALESCE(SUM(CASE WHEN direction = 'in' THEN quantity ELSE -quantity END),0)").Scan(&n).Error
	return n, err
}
func (a *App) products(c *gin.Context) {
	barcode := strings.TrimSpace(c.Query("barcode"))
	if barcode != "" {
		var p Product
		if err := a.db.Where("barcode = ?", barcode).First(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				c.JSON(200, gin.H{"product": nil, "stock": 0})
				return
			}
			fail(c, 500, "查询失败")
			return
		}
		n, err := balance(a.db, p.ID)
		if err != nil {
			fail(c, 500, "查询失败")
			return
		}
		c.JSON(200, gin.H{"product": p, "stock": n})
		return
	}
	type row struct {
		Product
		Stock        int64  `json:"stock"`
		CategoryName string `json:"category_name"`
	}
	rows := make([]row, 0)
	q := a.db.Table("products").Select("products.*, categories.name AS category_name, COALESCE(SUM(CASE WHEN records.direction = 'in' THEN records.quantity ELSE -records.quantity END),0) AS stock").Joins("LEFT JOIN categories ON categories.id = products.category_id").Joins("LEFT JOIN records ON records.product_id = products.id AND records.deleted_at IS NULL").Group("products.id")
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		q = q.Where("products.name LIKE ? OR products.barcode LIKE ?", "%"+s+"%", "%"+s+"%")
	}
	if err := q.Order("products.id desc").Limit(500).Scan(&rows).Error; err != nil {
		fail(c, 500, "查询失败")
		return
	}
	c.JSON(200, rows)
}
func (a *App) createRecord(c *gin.Context) {
	var in struct {
		Barcode    string `json:"barcode"`
		Name       string `json:"name"`
		CategoryID uint   `json:"category_id"`
		Unit       string `json:"unit"`
		Direction  string `json:"direction"`
		Quantity   int64  `json:"quantity"`
		Photo      string `json:"photo"`
		Note       string `json:"note"`
		Source     string `json:"source"`
		OccurredAt string `json:"occurred_at"`
	}
	if c.ShouldBindJSON(&in) != nil {
		fail(c, 400, "表单格式错误")
		return
	}
	in.Barcode = strings.TrimSpace(in.Barcode)
	in.Name = strings.TrimSpace(in.Name)
	in.Unit = strings.TrimSpace(in.Unit)
	in.Source = strings.TrimSpace(in.Source)
	if in.Source == "" || len([]rune(in.Source)) > 100 {
		fail(c, 400, "请填写来源，最多 100 个字符")
		return
	}
	occurredAt := time.Now().UTC()
	if in.OccurredAt != "" {
		var err error
		occurredAt, err = parseOperationTime(in.OccurredAt)
		if err != nil {
			fail(c, 400, "操作时间无效，请选择有效日期时间")
			return
		}
	}
	if in.CategoryID == 0 || in.Barcode == "" || len(in.Barcode) > 128 || in.Name == "" || len(in.Name) > 200 || in.Unit == "" || len(in.Unit) > 24 || (in.Direction != "in" && in.Direction != "out") || in.Quantity < 1 || in.Quantity > 1000000000 || len(in.Note) > 2000 {
		fail(c, 400, "请完整填写物品信息，数量应为 1–10 亿的整数")
		return
	}
	u := current(c)
	var record Record
	err := a.db.Transaction(func(tx *gorm.DB) error {
		var cat Category
		if tx.First(&cat, in.CategoryID).Error != nil {
			return errors.New("请选择有效分类")
		}
		if in.Photo != "" {
			var p Photo
			if tx.First(&p, "key = ? AND user_id = ?", in.Photo, u.ID).Error != nil {
				return errors.New("请重新上传图片")
			}
		}
		var p Product
		err := tx.Where("barcode = ?", in.Barcode).First(&p).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if in.Direction == "out" {
				return errors.New("物品尚未入库，无法出库")
			}
			p = Product{Barcode: in.Barcode, Name: in.Name, CategoryID: in.CategoryID, Unit: in.Unit}
			if err := tx.Create(&p).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if p.Name != in.Name || p.CategoryID != in.CategoryID || p.Unit != in.Unit {
			return errors.New("此条码已绑定其他物品信息，请使用已自动填入的信息")
		}
		n, err := balance(tx, p.ID)
		if err != nil {
			return err
		}
		if in.Direction == "out" && n < in.Quantity {
			return fmt.Errorf("库存不足，当前可用库存为 %d %s", n, p.Unit)
		}
		record = Record{ProductID: p.ID, CategoryID: p.CategoryID, UserID: u.ID, Direction: in.Direction, Quantity: in.Quantity, Photo: in.Photo, Note: strings.TrimSpace(in.Note), Source: in.Source, OccurredAt: occurredAt}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return audit(tx, u.ID, "record.create", fmt.Sprintf("记录 #%d %s %d", record.ID, in.Direction, in.Quantity))
	})
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	c.JSON(201, record)
}

func (a *App) filtered(c *gin.Context) (*gorm.DB, error) {
	q := a.db.Model(&Record{})
	if c.Query("trash") == "1" && current(c).Role == "admin" {
		q = q.Unscoped().Where("records.deleted_at IS NOT NULL")
	}
	if current(c).Role != "admin" {
		q = q.Where("records.user_id = ?", current(c).ID)
	}
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		q = q.Where("product_id IN (SELECT id FROM products WHERE name LIKE ? OR barcode LIKE ?)", "%"+s+"%", "%"+s+"%")
	}
	if v := c.Query("direction"); v != "" {
		q = q.Where("direction = ?", v)
	}
	if v := c.Query("category_id"); v != "" {
		q = q.Where("records.category_id = ?", v)
	}
	var start, end time.Time
	if value := c.Query("start"); value != "" {
		var err error
		start, err = parseOperationTime(value)
		if err != nil {
			return nil, errors.New("开始时间格式无效")
		}
		q = q.Where("records.occurred_at >= ?", start)
	}
	if value := c.Query("end"); value != "" {
		var err error
		end, err = parseOperationTime(value)
		if err != nil {
			return nil, errors.New("结束时间格式无效")
		}
		q = q.Where("records.occurred_at <= ?", end)
	}
	if !start.IsZero() && !end.IsZero() && end.Before(start) {
		return nil, errors.New("结束时间不能早于开始时间")
	}
	return q, nil
}
func (a *App) records(c *gin.Context) {
	q, err := a.filtered(c)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		page = 1000000
	}
	var total int64
	if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		fail(c, 500, "查询失败")
		return
	}
	var rows []Record
	if err := q.Preload("Product").Preload("Category").Preload("User").Order("records.occurred_at desc, records.id desc").Limit(20).Offset((page - 1) * 20).Find(&rows).Error; err != nil {
		fail(c, 500, "查询失败")
		return
	}
	c.JSON(200, gin.H{"items": rows, "total": total, "page": page, "page_size": 20})
}
func (a *App) stats(c *gin.Context) {
	var rows int64
	var incoming, outgoing int64
	base := func() *gorm.DB {
		q := a.db.Model(&Record{})
		if current(c).Role != "admin" {
			q = q.Where("user_id = ?", current(c).ID)
		}
		return q
	}
	base().Count(&rows)
	base().Where("direction = ?", "in").Select("COALESCE(SUM(quantity),0)").Scan(&incoming)
	base().Where("direction = ?", "out").Select("COALESCE(SUM(quantity),0)").Scan(&outgoing)
	c.JSON(200, gin.H{"records": rows, "incoming": incoming, "outgoing": outgoing})
}
func (a *App) createCategory(c *gin.Context) {
	var in Category
	if c.ShouldBindJSON(&in) != nil || strings.TrimSpace(in.Name) == "" || len(in.Name) > 64 {
		fail(c, 400, "分类名称不能为空且不超过 64 字节")
		return
	}
	in.ID = 0
	in.Name = strings.TrimSpace(in.Name)
	if err := a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&in).Error; err != nil {
			return err
		}
		return audit(tx, current(c).ID, "category.create", in.Name)
	}); err != nil {
		fail(c, 409, "分类已存在或保存失败")
		return
	}
	c.JSON(201, in)
}
func (a *App) deleteCategory(c *gin.Context) {
	err := a.db.Transaction(func(tx *gorm.DB) error {
		var cat Category
		if err := tx.First(&cat, c.Param("id")).Error; err != nil {
			return errors.New("分类不存在")
		}
		var n int64
		tx.Model(&Product{}).Where("category_id = ?", cat.ID).Count(&n)
		if n > 0 {
			return errors.New("分类已被物品使用，无法删除")
		}
		if err := tx.Delete(&cat).Error; err != nil {
			return err
		}
		return audit(tx, current(c).ID, "category.delete", cat.Name)
	})
	if err != nil {
		fail(c, 409, err.Error())
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
func (a *App) trash(c *gin.Context)   { a.changeTrash(c, false) }
func (a *App) restore(c *gin.Context) { a.changeTrash(c, true) }
func (a *App) changeTrash(c *gin.Context, restore bool) {
	err := a.db.Transaction(func(tx *gorm.DB) error {
		var r Record
		q := tx
		if restore {
			q = q.Unscoped().Where("deleted_at IS NOT NULL")
		}
		if err := q.First(&r, c.Param("id")).Error; err != nil {
			return errors.New("记录不存在或状态已变更")
		}
		n, err := balance(tx, r.ProductID)
		if err != nil {
			return err
		}
		delta := r.Quantity
		if r.Direction == "out" {
			delta = -delta
		}
		if !restore {
			delta = -delta
		}
		if n+delta < 0 {
			return errors.New("此操作会使库存变为负数，请先处理相关出库记录")
		}
		action := "record.trash"
		if restore {
			err = tx.Unscoped().Model(&r).Update("deleted_at", nil).Error
			action = "record.restore"
		} else {
			err = tx.Delete(&r).Error
		}
		if err != nil {
			return err
		}
		return audit(tx, current(c).ID, action, fmt.Sprintf("记录 #%d", r.ID))
	})
	if err != nil {
		fail(c, 409, err.Error())
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
func (a *App) emptyTrash(c *gin.Context) {
	err := a.db.Transaction(func(tx *gorm.DB) error {
		r := tx.Unscoped().Where("deleted_at IS NOT NULL").Delete(&Record{})
		if r.Error != nil {
			return r.Error
		}
		return audit(tx, current(c).ID, "trash.clear", fmt.Sprintf("永久删除 %d 条记录", r.RowsAffected))
	})
	if err != nil {
		fail(c, 500, "清空失败")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
func (a *App) upload(c *gin.Context) {
	f, header, err := c.Request.FormFile("photo")
	if err != nil {
		fail(c, 400, "请选择图片，且不要超过上传大小限制")
		return
	}
	defer f.Close()
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	limit := a.cfg.Storage.MaxUploadMB * 1024 * 1024
	if header.Size > limit {
		fail(c, 413, "图片过大")
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		fail(c, 413, "图片过大或读取失败")
		return
	}
	info, format, err := image.DecodeConfig(strings.NewReader(string(data)))
	if err != nil || info.Width < 1 || info.Height < 1 || int64(info.Width)*int64(info.Height) > 40000000 {
		fail(c, 400, "请上传有效 JPG、PNG 或 GIF 图片（最多 4000 万像素）")
		return
	}
	ext := map[string]string{"jpeg": ".jpg", "png": ".png", "gif": ".gif"}[format]
	if ext == "" {
		fail(c, 400, "不支持此图片格式")
		return
	}
	key := token() + ext
	path := filepath.Join(a.cfg.Storage.Uploads, key)
	if err = os.WriteFile(path, data, 0600); err != nil {
		fail(c, 500, "保存图片失败")
		return
	}
	if err = a.db.Create(&Photo{Key: key, UserID: current(c).ID}).Error; err != nil {
		_ = os.Remove(path)
		fail(c, 500, "保存图片失败")
		return
	}
	c.JSON(201, gin.H{"key": key, "url": "/uploads/" + key})
}
func (a *App) createUser(c *gin.Context) {
	var in struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		Role        string `json:"role"`
		DisplayName string `json:"display_name"`
	}
	if c.ShouldBindJSON(&in) != nil || len([]rune(strings.TrimSpace(in.DisplayName))) > 50 || len(strings.TrimSpace(in.Username)) < 2 || len(in.Username) > 64 || len(in.Password) < 12 || len(in.Password) > 72 || (in.Role != "admin" && in.Role != "user") {
		fail(c, 400, "账号至少 2 字符，密码 12–72 字节，角色须有效")
		return
	}
	h, _ := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	u := User{Username: strings.TrimSpace(in.Username), DisplayName: strings.TrimSpace(in.DisplayName), Password: string(h), Role: in.Role}
	err := a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		return audit(tx, current(c).ID, "user.create", u.Username)
	})
	if err != nil {
		fail(c, 409, "账号已存在或创建失败")
		return
	}
	c.JSON(201, u)
}
func (a *App) updateUser(c *gin.Context) {
	var in struct {
		Disabled    *bool   `json:"disabled"`
		DisplayName *string `json:"display_name"`
	}
	if c.ShouldBindJSON(&in) != nil {
		fail(c, 400, "参数错误")
		return
	}
	if in.DisplayName != nil && len([]rune(strings.TrimSpace(*in.DisplayName))) > 50 {
		fail(c, 400, "姓名最多 50 个字符")
		return
	}
	if in.DisplayName == nil && in.Disabled == nil {
		fail(c, 400, "未提供需要修改的字段")
		return
	}
	err := a.db.Transaction(func(tx *gorm.DB) error {
		var u User
		if tx.First(&u, c.Param("id")).Error != nil {
			return errors.New("账号不存在")
		}
		if in.Disabled != nil && u.ID == current(c).ID {
			return errors.New("不能禁用自己的账号")
		}
		if in.Disabled != nil && *in.Disabled && u.Role == "admin" && !u.Disabled {
			var activeAdmins int64
			if err := tx.Model(&User{}).Where("role = ? AND disabled = ?", "admin", false).Count(&activeAdmins).Error; err != nil {
				return err
			}
			if activeAdmins <= 1 {
				return errors.New("必须保留至少一个可用管理员")
			}
		}
		if in.DisplayName != nil {
			if err := tx.Model(&u).Update("display_name", strings.TrimSpace(*in.DisplayName)).Error; err != nil {
				return err
			}
			if err := audit(tx, current(c).ID, "user.profile", "设置姓名："+u.Username); err != nil {
				return err
			}
		}
		if in.Disabled != nil {
			if err := tx.Model(&u).Update("disabled", *in.Disabled).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ?", u.ID).Delete(&Session{}).Error; err != nil {
				return err
			}
			return audit(tx, current(c).ID, "user.status", fmt.Sprintf("%s disabled=%t", u.Username, *in.Disabled))
		}
		return nil
	})
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func main() {
	path := os.Getenv("INVENTORY_CONFIG")
	if path == "" {
		path = "config.yaml"
	}
	cfg, err := loadConfig(path)
	if err != nil {
		log.Fatal(err)
	}
	a, err := newApp(cfg)
	if err != nil {
		log.Fatal(err)
	}
	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{Addr: cfg.Server.Address, Handler: a.router(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		log.Printf("空谷服务监听：%s", cfg.Server.Address)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	sqlDB, _ := a.db.DB()
	_ = sqlDB.Close()
}
