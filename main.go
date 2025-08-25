package main

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"github.com/spf13/viper"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"html/template"
)

// 数据库连接
var db *gorm.DB

// Server 结构体，用于存储表中的数据
type Server struct {
	TableName        string
	ID               int
	Name             string
	Port             string
	ServerPort       int
	Host             string
	Show             bool
	NextUpdateTime   int64  `gorm:"column:next_update_time"`
	LastUpdateStatus string `gorm:"column:last_update_status"`
	DomainTotal      int
	DomainAvailable  int
}

// ServerDomain 结构体，用于存储每个服务器的域名
type ServerDomain struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	ServerTable  string `gorm:"column:server_table;type:varchar(255);not null" json:"server_table"`
	ServerID     int    `gorm:"column:server_id;not null" json:"server_id"`
	Domain       string `gorm:"column:domain;type:varchar(255);uniqueIndex:unique_domain_per_server;not null" json:"domain"`
	InUse        int8   `gorm:"type:tinyint;default:0" json:"in_use"`
	Order        int    `gorm:"not null" json:"order"`
	LastUsedTime int64  `gorm:"column:last_used_time;default:0" json:"last_used_time"`
}

// 全局变量
var updateIntervalHours = 24 // 默认更新间隔 24 小时
var minPort int
var maxPort int

func main() {
	// 加载配置文件
	viper.SetConfigName("config")
	viper.SetConfigType("toml")
	viper.AddConfigPath(".")
	if err := viper.ReadInConfig(); err != nil {
		log.Fatal("读取配置文件失败: ", err)
	}

	// 读取配置值
	dbUser := viper.GetString("database.user")
	dbPass := viper.GetString("database.password")
	dbHost := viper.GetString("database.host")
	dbPort := viper.GetString("database.port")
	dbName := viper.GetString("database.name")
	authUsername := viper.GetString("auth.username")
	authPassword := viper.GetString("auth.password")
	minPort = viper.GetInt("port.min")
	maxPort = viper.GetInt("port.max")
	updateIntervalHours = viper.GetInt("server.updateIntervalHours")
	// 验证端口范围
	if minPort >= maxPort {
		log.Fatal("端口范围无效：最小端口必须小于最大端口")
	}

	// 初始化数据库连接
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", dbUser, dbPass, dbHost, dbPort, dbName)
	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("数据库连接失败: ", err)
	}

	// 配置数据库连接池
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal("获取 sql.DB 失败: ", err)
	}
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	// 自动迁移 server_domains 表
	if err := db.AutoMigrate(&ServerDomain{}); err != nil {
		log.Fatal("自动迁移 server_domains 表失败: ", err)
	}

	// 为性能添加索引
	if err := db.Exec("CREATE INDEX idx_server_domains_all ON server_domains (server_table, server_id, last_used_time)").Error; err != nil {
		log.Printf("创建 server_domains 索引失败: %v", err)
	} else {
		log.Println("索引 idx_server_domains_all 已创建或已存在")
	}

	// 验证表创建
	var tableCount int64
	db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?", dbName, "server_domains").Scan(&tableCount)
	if tableCount == 0 {
		log.Fatal("server_domains 表未创建")
	} else {
		log.Println("server_domains 表验证或创建成功")
	}

	// 检查并添加列到服务器表
	addColumnIfNotExists("v2_server_vless", "next_update_time", "BIGINT DEFAULT 0")
	addColumnIfNotExists("v2_server_shadowsocks", "next_update_time", "BIGINT DEFAULT 0")
	addColumnIfNotExists("v2_server_vmess", "next_update_time", "BIGINT DEFAULT 0")
	addColumnIfNotExists("v2_server_vless", "last_update_status", "VARCHAR(255) DEFAULT ''")
	addColumnIfNotExists("v2_server_shadowsocks", "last_update_status", "VARCHAR(255) DEFAULT ''")
	addColumnIfNotExists("v2_server_vmess", "last_update_status", "VARCHAR(255) DEFAULT ''")

	// 初始化示例数据
	initSampleData()

	// 初始化已使用资源
	initUsedResources()

	// 设置 Gin 路由
	r := gin.Default()

	// 设置信任的代理（修复警告）
	r.SetTrustedProxies([]string{"127.0.0.1"}) // 根据需要调整

	// 设置会话中间件
	store := cookie.NewStore([]byte("secret123"))
	store.Options(sessions.Options{
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	r.Use(sessions.Sessions("mysession", store))

	// 提供静态文件
	r.Static("/static", "./static")

	// 定义自定义模板函数
	funcMap := template.FuncMap{
		"formatUnixTime": func(timestamp int64) string {
			if timestamp == 0 {
				return "立即更新"
			}
			return time.Unix(timestamp, 0).Format("2006-01-02 15:04:05")
		},
		"formatDomainCount": func(total, available int) string {
			return fmt.Sprintf("%d/%d", total, available)
		},
	}

	// 加载 HTML 模板并应用自定义函数
	r.SetFuncMap(funcMap)
	r.LoadHTMLGlob("templates/*")

	// 根路径重定向到 /login
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/login")
	})

	// 登录页面
	r.GET("/login", func(c *gin.Context) {
		c.HTML(http.StatusOK, "login.html", nil)
	})

	// 登录处理
	r.POST("/login", func(c *gin.Context) {
		username := c.PostForm("username")
		password := c.PostForm("password")
		if username == authUsername && password == authPassword {
			session := sessions.Default(c)
			session.Set("user", username)
			if err := session.Save(); err != nil {
				log.Printf("保存会话失败: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "保存会话失败"})
				return
			}
			c.Redirect(http.StatusFound, "/servers")
			return
		}
		c.HTML(http.StatusUnauthorized, "login.html", gin.H{"error": "无效的用户名或密码"})
	})

	// 登出
	r.GET("/logout", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Clear()
		session.Save()
		c.Redirect(http.StatusFound, "/login")
	})

	// 服务器列表
	r.GET("/servers", authMiddleware, func(c *gin.Context) {
		var servers []Server
		tables := []string{"v2_server_vless", "v2_server_shadowsocks", "v2_server_vmess"}
		for _, table := range tables {
			var records []struct {
				ID               int
				Name             string
				Port             string
				ServerPort       int
				Host             string
				Show             bool
				NextUpdateTime   int64
				LastUpdateStatus string
			}
			if err := db.Table(table).Select("id, name, port, server_port, host, `show`, next_update_time, last_update_status").Find(&records).Error; err != nil {
				log.Printf("从表 %s 获取记录失败: %v", table, err)
				continue
			}
			for _, s := range records {
				var total int64
				db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ?", table, s.ID).Count(&total)
				var available int64
				db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ? AND in_use = ? AND (last_used_time = 0 OR last_used_time <= ?)", table, s.ID, 0, time.Now().Unix()-3*3600).Count(&available)
				servers = append(servers, Server{
					TableName:        table,
					ID:               s.ID,
					Name:             s.Name,
					Port:             s.Port,
					ServerPort:       s.ServerPort,
					Host:             s.Host,
					Show:             s.Show,
					NextUpdateTime:   s.NextUpdateTime,
					LastUpdateStatus: s.LastUpdateStatus,
					DomainTotal:      int(total),
					DomainAvailable:  int(available),
				})
			}
		}
		c.HTML(http.StatusOK, "servers.html", gin.H{"Servers": servers, "Interval": updateIntervalHours, "MinPort": minPort, "MaxPort": maxPort})
	})

	// 获取所有域名（包括已使用和未使用）
	r.GET("/available-domains", authMiddleware, func(c *gin.Context) {
		table := c.Query("table")
		idStr := c.Query("id")
		id, err := strconv.Atoi(idStr)
		if err != nil || id <= 0 {
			log.Printf("无效的ID: %s", idStr)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的ID"})
			return
		}
		validTables := []string{"v2_server_vless", "v2_server_shadowsocks", "v2_server_vmess"}
		isValidTable := false
		for _, t := range validTables {
			if t == table {
				isValidTable = true
				break
			}
		}
		if !isValidTable {
			log.Printf("无效的表名: %s", table)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的表名"})
			return
		}
		var domains []ServerDomain
		err = db.Select("id, server_table, server_id, domain, in_use, `order`, last_used_time").
			Where("server_table = ? AND server_id = ?", table, id).
			Order("last_used_time ASC").Find(&domains).Error
		if err != nil {
			log.Printf("获取表 %s, ID %d 的域名失败: %v", table, id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取域名列表: " + err.Error()})
			return
		}
		log.Printf("为表 %s, ID %d 获取到 %d 个域名", table, id, len(domains))
		for _, d := range domains {
			log.Printf("域名: %s, in_use=%d, last_used_time=%d", d.Domain, d.InUse, d.LastUsedTime)
		}
		c.JSON(http.StatusOK, gin.H{"domains": domains})
	})

	// 添加新域名
	r.POST("/add-domain", authMiddleware, func(c *gin.Context) {
		table := c.PostForm("table")
		idStr := c.PostForm("id")
		domain := c.PostForm("domain")
		id, err := strconv.Atoi(idStr)
		if err != nil || id <= 0 {
			log.Printf("无效的ID: %s", idStr)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的ID"})
			return
		}
		validTables := []string{"v2_server_vless", "v2_server_shadowsocks", "v2_server_vmess"}
		isValidTable := false
		for _, t := range validTables {
			if t == table {
				isValidTable = true
				break
			}
		}
		if !isValidTable {
			log.Printf("无效的表名: %s", table)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的表名"})
			return
		}
		if domain == "" {
			log.Printf("无效的域名: 为空")
			c.JSON(http.StatusBadRequest, gin.H{"error": "域名不能为空"})
			return
		}
		var existingDomain ServerDomain
		if err := db.Where("server_table = ? AND server_id = ? AND domain = ?", table, id, domain).First(&existingDomain).Error; err == nil {
			log.Printf("域名已存在: 表=%s, ID=%d, 域名=%s", table, id, domain)
			c.JSON(http.StatusBadRequest, gin.H{"error": "域名已存在"})
			return
		}
		var maxOrder int
		db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ?", table, id).Select("MAX(`order`)").Scan(&maxOrder)
		newDomain := ServerDomain{
			ServerTable:  table,
			ServerID:     id,
			Domain:       domain,
			InUse:        0,
			Order:        maxOrder + 1,
			LastUsedTime: 0,
		}
		if err := db.Create(&newDomain).Error; err != nil {
			log.Printf("添加域名 %s 失败: 表=%s, ID=%d, 错误=%v", domain, table, id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "添加域名失败：" + err.Error()})
			return
		}
		var total int64
		db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ?", table, id).Count(&total)
		var available int64
		db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ? AND in_use = ? AND (last_used_time = 0 OR last_used_time <= ?)", table, id, 0, time.Now().Unix()-3*3600).Count(&available)
		c.JSON(http.StatusOK, gin.H{
			"message":          "域名 " + domain + " 添加成功",
			"domain_total":     total,
			"domain_available": available,
		})
	})

	// 删除域名
	r.POST("/delete-domain", authMiddleware, func(c *gin.Context) {
		table := c.PostForm("table")
		idStr := c.PostForm("id")
		domainIDStr := c.PostForm("domain_id")
		id, err := strconv.Atoi(idStr)
		if err != nil || id <= 0 {
			log.Printf("无效的服务器ID: %s", idStr)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的服务器ID"})
			return
		}
		domainID, err := strconv.Atoi(domainIDStr)
		if err != nil || domainID <= 0 {
			log.Printf("无效的域名ID: %s", domainIDStr)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的域名ID"})
			return
		}
		validTables := []string{"v2_server_vless", "v2_server_shadowsocks", "v2_server_vmess"}
		isValidTable := false
		for _, t := range validTables {
			if t == table {
				isValidTable = true
				break
			}
		}
		if !isValidTable {
			log.Printf("无效的表名: %s", table)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的表名"})
			return
		}
		var domain ServerDomain
		if err := db.Where("id = ? AND server_table = ? AND server_id = ?", domainID, table, id).First(&domain).Error; err != nil {
			log.Printf("域名不存在: ID=%d, 表=%s, 服务器ID=%d, 错误=%v", domainID, table, id, err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "域名不存在"})
			return
		}
		if domain.InUse == 1 {
			log.Printf("无法删除正在使用的域名: ID=%d, 域名=%s, 表=%s, 服务器ID=%d", domainID, domain.Domain, table, id)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无法删除正在使用的域名"})
			return
		}
		var currentServer struct {
			Host string
		}
		if err := db.Table(table).Select("host").Where("id = ?", id).First(&currentServer).Error; err == nil && currentServer.Host == domain.Domain {
			log.Printf("无法删除当前服务器使用的域名: 域名=%s, 表=%s, ID=%d", domain.Domain, table, id)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无法删除当前服务器使用的域名"})
			return
		}
		if err := db.Delete(&ServerDomain{}, "id = ? AND server_table = ? AND server_id = ?", domainID, table, id).Error; err != nil {
			log.Printf("删除域名失败: ID=%d, 表=%s, 服务器ID=%d, 错误=%v", domainID, table, id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除域名失败：" + err.Error()})
			return
		}
		var total int64
		db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ?", table, id).Count(&total)
		var available int64
		db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ? AND in_use = ? AND (last_used_time = 0 OR last_used_time <= ?)", table, id, 0, time.Now().Unix()-3*3600).Count(&available)
		c.JSON(http.StatusOK, gin.H{
			"message":          "域名 " + domain.Domain + " 删除成功",
			"domain_total":     total,
			"domain_available": available,
		})
	})

	// 设置更新间隔
	r.POST("/set-interval", authMiddleware, func(c *gin.Context) {
		intervalStr := c.PostForm("interval")
		interval, err := strconv.Atoi(intervalStr)
		if err != nil || interval <= 0 {
			log.Printf("无效的间隔: %s", intervalStr)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的间隔"})
			return
		}
		viper.Set("server.updateIntervalHours", interval)
		updateIntervalHours = interval
		now := time.Now().Unix()
		newNextUpdateTime := now + int64(interval*3600)
		tables := []string{"v2_server_vless", "v2_server_shadowsocks", "v2_server_vmess"}
		for _, table := range tables {
			if err := db.Table(table).Where("1 = 1").Update("next_update_time", newNextUpdateTime).Error; err != nil {
				log.Printf("更新表 %s 的 next_update_time 失败: %v", table, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "更新间隔失败：" + err.Error()})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"message": "更新间隔已设置为 " + intervalStr + " 小时，所有服务器下次更新时间已刷新"})
	})

	// 立即更新服务器
	r.POST("/update-now", authMiddleware, func(c *gin.Context) {
		table := c.PostForm("table")
		idStr := c.PostForm("id")
		id, err := strconv.Atoi(idStr)
		if err != nil || id <= 0 {
			log.Printf("无效的ID: %s", idStr)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的ID"})
			return
		}
		validTables := []string{"v2_server_vless", "v2_server_shadowsocks", "v2_server_vmess"}
		isValidTable := false
		for _, t := range validTables {
			if t == table {
				isValidTable = true
				break
			}
		}
		if !isValidTable {
			log.Printf("无效的表名: %s", table)
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的表名"})
			return
		}
		now := time.Now().Unix()
		if err := updateServer(table, id, now, false); err != nil {
			log.Printf("更新服务器失败: 表=%s, ID=%d, 错误=%v", table, id, err)
			if updateErr := db.Table(table).Where("id = ?", id).Update("last_update_status", "更新失败："+err.Error()).Error; updateErr != nil {
				log.Printf("更新 last_update_status 失败: 表=%s, ID=%d, 错误=%v", table, id, updateErr)
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败：" + err.Error()})
			return
		}
		if err := db.Table(table).Where("id = ?", id).Update("last_update_status", "更新成功").Error; err != nil {
			log.Printf("更新 last_update_status 失败: 表=%s, ID=%d, 错误=%v", table, id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败：" + err.Error()})
			return
		}
		var server struct {
			Port             string
			Host             string
			NextUpdateTime   int64
			LastUpdateStatus string
		}
		if err := db.Table(table).Select("port, host, next_update_time, last_update_status").Where("id = ?", id).First(&server).Error; err != nil {
			log.Printf("获取更新后的服务器失败: 表=%s, ID=%d, 错误=%v", table, id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取更新后的服务器数据"})
			return
		}
		var total int64
		db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ?", table, id).Count(&total)
		var available int64
		db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ? AND in_use = ? AND (last_used_time = 0 OR last_used_time <= ?)", table, id, 0, time.Now().Unix()-3*3600).Count(&available)
		log.Printf("更新服务器成功: 表=%s, ID=%d, 端口=%s, 主机=%s, 域名总数=%d, 可用域名=%d", table, id, server.Port, server.Host, total, available)
		c.JSON(http.StatusOK, gin.H{
			"message":            "服务器已立即更新",
			"port":               server.Port,
			"host":               server.Host,
			"next_update_time":   server.NextUpdateTime,
			"last_update_status": server.LastUpdateStatus,
			"domain_total":       total,
			"domain_available":   available,
		})
	})

	// 设置端口范围
	r.POST("/set-port-range", authMiddleware, func(c *gin.Context) {
		minStr := c.PostForm("min_port")
		maxStr := c.PostForm("max_port")
		min, err := strconv.Atoi(minStr)
		if err != nil || min <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的最小端口"})
			return
		}
		max, err := strconv.Atoi(maxStr)
		if err != nil || max <= 0 || max <= min {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的最大端口"})
			return
		}
		minPort = min
		maxPort = max
		viper.Set("port.min", min)
		viper.Set("port.max", max)
		if err := viper.WriteConfig(); err != nil {
			log.Printf("写入配置文件失败: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存端口范围失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "端口范围已更新"})
	})

	// 调试：检查所有域名
	r.GET("/debug-domains", authMiddleware, func(c *gin.Context) {
		var domains []ServerDomain
		db.Find(&domains)
		c.JSON(http.StatusOK, gin.H{"all_domains": domains})
	})

	// 启动 cron 任务
	c := cron.New()
	c.AddFunc("*/5 * * * *", checkAndUpdateServers)
	c.Start()

	// 启动服务
	serAddr := viper.GetString("Server.Addr")
	log.Printf("启动服务于 %s", serAddr)
	if err := r.Run(serAddr); err != nil {
		log.Fatal("服务启动失败:", err)
	}
}

// 检查并添加列
func addColumnIfNotExists(table, column, columnType string) {
	var count int64
	db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = ?", "vpn", table, column).Scan(&count)
	if count == 0 {
		if err := db.Exec("ALTER TABLE " + table + " ADD " + column + " " + columnType).Error; err != nil {
			log.Printf("向表 %s 添加列 %s 失败: %v", table, column, err)
		} else {
			log.Printf("向表 %s 添加列 %s 成功", table, column)
		}
	}
}

// 初始化示例数据
func initSampleData() {
	var domainCount int64
	db.Model(&ServerDomain{}).Count(&domainCount)
	if domainCount > 0 {
		log.Println("server_domains 表已有数据，跳过示例数据初始化")
		return
	}
	tables := []string{"v2_server_vless", "v2_server_shadowsocks", "v2_server_vmess"}
	domains := []string{"domain1.com", "domain2.com", "domain3.com", "domain4.com", "321sds.com"}
	for _, table := range tables {
		var serverCount int64
		db.Table(table).Count(&serverCount)
		if serverCount == 0 {
			log.Printf("表 %s 无数据，插入示例服务器", table)
			db.Exec(fmt.Sprintf("INSERT INTO %s (id, name, port, server_port, host, `show`) VALUES (4, '%sServer4', '8080', 8080, '', 1)", table, table))
		}
		var serverIDs []int
		db.Table(table).Select("id").Find(&serverIDs)
		for _, serverID := range serverIDs {
			for i, d := range domains {
				var existingDomain ServerDomain
				if err := db.Where("server_table = ? AND server_id = ? AND domain = ?", table, serverID, d).First(&existingDomain).Error; err == nil {
					continue
				}
				if err := db.Create(&ServerDomain{
					ServerTable:  table,
					ServerID:     serverID,
					Domain:       d,
					InUse:        0,
					Order:        i + 1,
					LastUsedTime: 0,
				}).Error; err != nil {
					log.Printf("插入示例域名 %s 失败: 表=%s, 服务器ID=%d, 错误=%v", d, table, serverID, err)
				}
			}
		}
	}
	log.Println("server_domains 示例数据初始化完成")
}

// 初始化已使用资源
func initUsedResources() {
	if err := db.Model(&ServerDomain{}).Updates(map[string]interface{}{"in_use": 0, "last_used_time": 0}).Error; err != nil {
		log.Printf("重置 server_domains 失败: %v", err)
	}
	tables := []string{"v2_server_vless", "v2_server_shadowsocks", "v2_server_vmess"}
	for _, table := range tables {
		var records []struct {
			ID   int
			Host string
		}
		db.Table(table).Select("id, host").Find(&records)
		for _, r := range records {
			if r.Host != "" {
				if err := db.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ? AND domain = ?", table, r.ID, r.Host).Updates(map[string]interface{}{
					"in_use":         1,
					"last_used_time": time.Now().Unix(),
				}).Error; err != nil {
					log.Printf("标记域名 %s 为已使用失败: 表=%s, ID=%d, 错误=%v", r.Host, table, r.ID, err)
				}
			}
		}
	}
	log.Println("已使用资源初始化完成")
}

// 检查并更新服务器
func checkAndUpdateServers() {
	log.Println("运行 checkAndUpdateServers，时间:", time.Now().Format("2006-01-02 15:04:05"))
	now := time.Now().Unix()
	tables := []string{"v2_server_vless", "v2_server_shadowsocks", "v2_server_vmess"}
	for _, table := range tables {
		var servers []struct {
			ID             int
			NextUpdateTime int64
		}
		if err := db.Table(table).Where("next_update_time <= ?", now).Find(&servers).Error; err != nil {
			log.Printf("从表 %s 获取服务器失败: %v", table, err)
			continue
		}
		for _, s := range servers {
			var err error
			for attempt := 0; attempt < 3; attempt++ {
				err = updateServer(table, s.ID, now, true)
				if err == nil {
					if updateErr := db.Table(table).Where("id = ?", s.ID).Update("last_update_status", "更新成功").Error; updateErr != nil {
						log.Printf("更新表 %s, ID=%d 的 last_update_status 失败: %v", table, s.ID, updateErr)
					}
					break
				}
				log.Printf("尝试 %d 更新服务器失败: 表=%s, ID=%d, 错误=%v", attempt+1, table, s.ID, err)
			}
			if err != nil {
				log.Printf("三次尝试后更新服务器失败: 表=%s, ID=%d, 错误=%v", table, s.ID, err)
				if updateErr := db.Table(table).Where("id = ?", s.ID).Updates(map[string]interface{}{
					"last_update_status": "更新失败：" + err.Error(),
					"next_update_time":   now + int64(updateIntervalHours*3600),
				}).Error; updateErr != nil {
					log.Printf("更新表 %s, ID=%d 的 last_update_status 失败: %v", table, s.ID, updateErr)
				}
			}
		}
	}
}

// 更新单个服务器
func updateServer(table string, id int, now int64, useOrder bool) error {
	log.Printf("开始 updateServer: 表=%s, ID=%d, 当前时间=%d, 使用顺序=%v", table, id, now, useOrder)

	tx := db.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			log.Printf("updateServer 发生恐慌: 表=%s, ID=%d, 错误=%v", table, id, r)
		}
	}()

	// 获取当前服务器信息
	var currentServer struct {
		Port       string
		ServerPort int
		Host       string
	}
	if err := tx.Table(table).Select("port, server_port, host").Where("id = ?", id).First(&currentServer).Error; err != nil {
		tx.Rollback()
		log.Printf("获取当前服务器失败: 表=%s, ID=%d, 错误=%v", table, id, err)
		return fmt.Errorf("获取服务器数据失败: %v", err)
	}
	log.Printf("当前服务器: 表=%s, ID=%d, 端口=%s, 服务器端口=%d, 主机=%s",
		table, id, currentServer.Port, currentServer.ServerPort, currentServer.Host)

	// 释放当前域名（如果存在），仅设置 in_use=0，不重置 last_used_time
	if currentServer.Host != "" {
		var domainCount int64
		tx.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ? AND domain = ?", table, id, currentServer.Host).Count(&domainCount)
		if domainCount == 0 {
			log.Printf("警告: 当前主机 %s 在 server_domains 中未找到: 表=%s, ID=%d", currentServer.Host, table, id)
		} else {
			if err := tx.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ? AND domain = ?", table, id, currentServer.Host).Update("in_use", 0).Error; err != nil {
				tx.Rollback()
				log.Printf("释放域名 %s 失败: 表=%s, ID=%d, 错误=%v", currentServer.Host, table, id, err)
				return fmt.Errorf("释放域名失败: %v", err)
			}
			log.Printf("释放域名 %s 成功: 表=%s, ID=%d", currentServer.Host, table, id)
		}
	}

	// 获取新的随机端口
	currentPort := currentServer.ServerPort
	var nextPort int
	for i := 0; i < 100; i++ {
		nextPort = rand.Intn(maxPort-minPort+1) + minPort
		if nextPort != currentPort {
			break
		}
		if i == 99 {
			tx.Rollback()
			log.Printf("无法找到不同的端口: 表=%s, ID=%d", table, id)
			return errors.New("无法找到不同的端口")
		}
	}
	log.Printf("选择新端口: %d, 表=%s, ID=%d", nextPort, table, id)

	// 获取可用域名，按 last_used_time 升序排序
	var availableDomains []ServerDomain
	domainQuery := tx.Select("id, server_table, server_id, domain, in_use, `order`, last_used_time").
		Where("server_table = ? AND server_id = ? AND in_use = ? AND (last_used_time = 0 OR last_used_time <= ?)",
			table, id, 0, now-3*3600)
	if currentServer.Host != "" {
		domainQuery = domainQuery.Where("domain != ?", currentServer.Host)
	}
	domainQuery = domainQuery.Order("last_used_time ASC")
	if err := domainQuery.Find(&availableDomains).Error; err != nil {
		tx.Rollback()
		log.Printf("获取可用域名失败: 表=%s, ID=%d, 错误=%v", table, id, err)
		return fmt.Errorf("获取可用域名失败: %v", err)
	}
	log.Printf("可用域名数: %v, 表=%s, ID=%d", len(availableDomains), table, id)
	for _, d := range availableDomains {
		log.Printf("可用域名: %s, in_use=%d, last_used_time=%d", d.Domain, d.InUse, d.LastUsedTime)
	}
	if len(availableDomains) == 0 {
		tx.Rollback()
		log.Printf("无可用域名（排除当前主机）: 表=%s, ID=%d", table, id)
		return errors.New("无可用域名")
	}

	// 选择第一个域名（last_used_time 最小）
	nextDomain := availableDomains[0]
	log.Printf("选择新域名: %s, 表=%s, ID=%d, last_used_time=%d", nextDomain.Domain, table, id, nextDomain.LastUsedTime)

	// 更新服务器记录
	updateFields := map[string]interface{}{
		"port":             strconv.Itoa(nextPort),
		"server_port":      nextPort,
		"host":             nextDomain.Domain,
		"next_update_time": now + int64(updateIntervalHours*3600),
	}
	if err := tx.Table(table).Where("id = ?", id).Updates(updateFields).Error; err != nil {
		tx.Rollback()
		log.Printf("更新服务器记录失败: 表=%s, ID=%d, 错误=%v", table, id, err)
		return fmt.Errorf("更新服务器记录失败: %v", err)
	}
	log.Printf("更新服务器记录成功: 表=%s, ID=%d, 端口=%s, 主机=%s, 下次更新时间=%d", table, id, updateFields["port"], nextDomain.Domain, now+int64(updateIntervalHours*3600))

	// 标记新域名为已使用，并更新 last_used_time
	if err := tx.Model(&ServerDomain{}).Where("id = ?", nextDomain.ID).Updates(map[string]interface{}{
		"in_use":         1,
		"last_used_time": now,
	}).Error; err != nil {
		tx.Rollback()
		log.Printf("标记域名 %s 为已使用失败: 表=%s, ID=%d, 错误=%v", nextDomain.Domain, table, id, err)
		return fmt.Errorf("标记域名失败: %v", err)
	}
	log.Printf("标记域名 %s 为已使用成功: 表=%s, ID=%d, last_used_time=%d", nextDomain.Domain, table, id, now)

	// 如果是 cron 任务，更新域名顺序
	if useOrder {
		var maxDomainOrder int
		tx.Model(&ServerDomain{}).Where("server_table = ? AND server_id = ?", table, id).Select("MAX(`order`)").Scan(&maxDomainOrder)
		if err := tx.Model(&ServerDomain{}).Where("id = ?", nextDomain.ID).Update("order", maxDomainOrder+1).Error; err != nil {
			tx.Rollback()
			log.Printf("更新域名顺序失败: 表=%s, ID=%d, 错误=%v", table, id, err)
			return fmt.Errorf("更新域名顺序失败: %v", err)
		}
		log.Printf("更新域名顺序到 %d: 域名=%s, 表=%s, ID=%d", maxDomainOrder+1, nextDomain.Domain, table, id)
	}

	// 提交事务
	if err := tx.Commit().Error; err != nil {
		log.Printf("提交事务失败: 表=%s, ID=%d, 错误=%v", table, id, err)
		return fmt.Errorf("事务提交失败: %v", err)
	}
	log.Printf("事务提交成功: 表=%s, ID=%d", table, id)

	// 调试：查询更新后的域名状态
	var updatedDomain ServerDomain
	if err := db.Where("server_table = ? AND server_id = ? AND domain = ?", table, id, nextDomain.Domain).First(&updatedDomain).Error; err != nil {
		log.Printf("查询更新后的域名失败: 表=%s, ID=%d, 域名=%s, 错误=%v", table, id, nextDomain.Domain, err)
	} else {
		log.Printf("更新后域名状态: 表=%s, ID=%d, 域名=%s, in_use=%d, last_used_time=%d", table, id, updatedDomain.Domain, updatedDomain.InUse, updatedDomain.LastUsedTime)
	}

	return nil
}

// 认证中间件
func authMiddleware(c *gin.Context) {
	session := sessions.Default(c)
	user := session.Get("user")
	if user == nil {
		c.Redirect(http.StatusFound, "/login")
		c.Abort()
		return
	}
	c.Next()
}
