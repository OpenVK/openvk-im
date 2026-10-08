package db

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	env "ovk-im/src/config"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var Instance *gorm.DB

func Connect() {
	user := env.Get("DB_USER", "root")
	pass := env.Get("DB_PASS", "")
	host := env.Get("DB_HOST", "127.0.0.1")
	port := env.Get("DB_PORT", "3306")
	name := env.Get("DB_NAME", "openvk_im")
	socket := strings.TrimSpace(env.Get("DB_SOCKET", ""))

	var dsnNoDb, dsn string
	if socket != "" {
		dsnNoDb = fmt.Sprintf("%s:%s@unix(%s)/?charset=utf8mb4&parseTime=True&loc=Local", user, pass, socket)
		dsn = fmt.Sprintf("%s:%s@unix(%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", user, pass, socket, name)
	} else {
		dsnNoDb = fmt.Sprintf("%s:%s@tcp(%s:%s)/?charset=utf8mb4&parseTime=True&loc=Local", user, pass, host, port)
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", user, pass, host, port, name)
	}

	tmpDb, err := sql.Open("mysql", dsnNoDb)
	if err != nil {
		log.Fatalf("Failed to connect to MySQL server: %v", err)
	}

	_, err = tmpDb.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;", name))
	if err != nil {
		log.Fatalf("Failed to create database: %v", err)
	}
	tmpDb.Close()

	logLevel := logger.Error
	if env.IsDev() {
		logLevel = logger.Info
	}

	dbConn, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger:                                   logger.Default.LogMode(logLevel),
		DisableForeignKeyConstraintWhenMigrating: true,
	})

	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	Instance = dbConn
	if socket != "" {
		log.Printf("Database '%s' connected via socket '%s'\n", name, socket)
	} else {
		log.Printf("Database '%s' connected via %s:%s\n", name, host, port)
	}
}
