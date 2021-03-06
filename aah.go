// Copyright (c) Jeevanandam M. (https://github.com/jeevatkm)
// go-aah/aah source code and usage is governed by a MIT style
// license that can be found in the LICENSE file.

// Package aah is A scalable, performant, rapid development Web framework for Go
// https://aahframework.org
package aah

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"aahframework.org/aruntime.v0"
	"aahframework.org/config.v0"
	"aahframework.org/essentials.v0"
	"aahframework.org/log.v0"
)

// Version no. of aah framework
const Version = "0.5"

// aah application variables
var (
	appName               string
	appDesc               string
	appImportPath         string
	appProfile            string
	appBaseDir            string
	appIsPackaged         bool
	appHTTPReadTimeout    time.Duration
	appHTTPWriteTimeout   time.Duration
	appHTTPMaxHdrBytes    int
	appSSLCert            string
	appSSLKey             string
	appIsLetsEncrypt      bool
	appMultipartMaxMemory int64
	appPID                int
	appInitialized        bool
	appBuildInfo          *BuildInfo
	appEngine             *engine

	appDefaultProfile        = "dev"
	appProfilePrefix         = "env."
	appDefaultHTTPPort       = "8080"
	appDefaultDateFormat     = "2006-01-02"
	appDefaultDateTimeFormat = "2006-01-02 15:04:05"

	goPath   string
	goSrcDir string
)

// BuildInfo holds the aah application build information; such as BinaryName,
// Version and Date.
type BuildInfo struct {
	BinaryName string
	Version    string
	Date       string
}

//‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾
// Global methods
//___________________________________

// AppName method returns aah application name from app config otherwise app name
// of the base directory.
func AppName() string {
	return appName
}

// AppDesc method returns aah application friendly description from app config
// otherwise empty string.
func AppDesc() string {
	return appDesc
}

// AppProfile returns aah application configuration profile name
// For e.g.: dev, prod, etc. Default is `dev`
func AppProfile() string {
	return appProfile
}

// AppBaseDir method returns the application base or binary current directory
// 	For e.g.:
// 		$GOPATH/src/github.com/user/myproject
// 		<app/binary/path/base/directory>
func AppBaseDir() string {
	return appBaseDir
}

// AppImportPath method returns the application Go import path.
func AppImportPath() string {
	return appImportPath
}

// AppHTTPAddress method returns aah application HTTP address otherwise empty string
func AppHTTPAddress() string {
	return AppConfig().StringDefault("server.address", "")
}

// AppHTTPPort method returns aah application HTTP port number based on `server.port`
// value. Possible outcomes are user-defined port, `80`, `443` and `8080`.
func AppHTTPPort() string {
	port := AppConfig().StringDefault("server.port", appDefaultHTTPPort)
	if !ess.IsStrEmpty(port) {
		return port
	}

	if AppIsSSLEnabled() {
		return "443"
	}

	return "80"
}

// AppDateFormat method returns aah application date format
func AppDateFormat() string {
	return AppConfig().StringDefault("format.date", appDefaultDateFormat)
}

// AppDateTimeFormat method returns aah application date format
func AppDateTimeFormat() string {
	return AppConfig().StringDefault("format.datetime", appDefaultDateTimeFormat)
}

// AppBuildInfo method return user application version no.
func AppBuildInfo() *BuildInfo {
	return appBuildInfo
}

// AllAppProfiles method returns all the aah application environment profile names.
func AllAppProfiles() []string {
	var profiles []string

	for _, v := range AppConfig().KeysByPath("env") {
		if v == "default" {
			continue
		}
		profiles = append(profiles, v)
	}

	return profiles
}

// AppIsSSLEnabled method returns true if aah application is enabled with SSL
// otherwise false.
func AppIsSSLEnabled() bool {
	return AppConfig().BoolDefault("server.ssl.enable", false)
}

// SetAppProfile method sets given profile as current aah application profile.
//		For Example:
//
//		aah.SetAppProfile("prod")
func SetAppProfile(profile string) error {
	if err := AppConfig().SetProfile(appProfilePrefix + profile); err != nil {
		return err
	}

	appProfile = profile
	return nil
}

// SetAppBuildInfo method sets the user application build info into aah instance.
func SetAppBuildInfo(bi *BuildInfo) {
	appBuildInfo = bi
}

// Init method initializes `aah` application, if anything goes wrong during
// initialize process, it will log it as fatal msg and exit.
func Init(importPath string) {
	defer aahRecover()

	logAsFatal(initPath(importPath))
	logAsFatal(initConfig(appConfigDir()))

	if appBuildInfo == nil {
		// aah CLI is accessing application for build purpose
		log.SetLevel(log.LevelWarn)
		logAsFatal(initAppVariables())
		logAsFatal(initRoutes(appConfigDir(), AppConfig()))
		log.SetLevel(log.LevelDebug)
	} else {
		// publish `OnInit` server event
		AppEventStore().sortAndPublishSync(&Event{Name: EventOnInit})

		logAsFatal(initAppVariables())
		logAsFatal(initLogs(appLogsDir(), AppConfig()))
		logAsFatal(initI18n(appI18nDir()))
		logAsFatal(initRoutes(appConfigDir(), AppConfig()))
		logAsFatal(initSecurity(appConfigDir(), AppConfig()))
		logAsFatal(initViewEngine(appViewsDir(), AppConfig()))
	}

	appInitialized = true
}

//‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾
// Unexported methods
//___________________________________

func aahRecover() {
	if r := recover(); r != nil {
		strace := aruntime.NewStacktrace(r, AppConfig())
		buf := &bytes.Buffer{}
		strace.Print(buf)

		log.Error("Recovered from panic:")
		log.Error(buf.String())
	}
}

func appLogsDir() string {
	return filepath.Join(AppBaseDir(), "logs")
}

func logAsFatal(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func initPath(importPath string) (err error) {
	appImportPath = path.Clean(importPath)
	appIsPackaged = isBinDirExists() && !isAppDirExists()

	if goPath, err = ess.GoPath(); err != nil && !appIsPackaged {
		return err
	}

	goSrcDir = filepath.Join(goPath, "src")
	appBaseDir = filepath.Join(goSrcDir, filepath.FromSlash(appImportPath))
	if appIsPackaged {
		appBaseDir = getWorkingDir()
	}

	if !ess.IsFileExists(appBaseDir) {
		return fmt.Errorf("aah application does not exists: %s", appImportPath)
	}

	return nil
}

func initAppVariables() error {
	var err error
	cfg := AppConfig()

	appName = cfg.StringDefault("name", filepath.Base(AppBaseDir()))
	appDesc = cfg.StringDefault("desc", "")

	appProfile = cfg.StringDefault("env.active", appDefaultProfile)
	if err = SetAppProfile(AppProfile()); err != nil {
		return err
	}

	readTimeout := cfg.StringDefault("server.timeout.read", "90s")
	writeTimeout := cfg.StringDefault("server.timeout.write", "90s")
	if !isValidTimeUnit(readTimeout, "s", "m") || !isValidTimeUnit(writeTimeout, "s", "m") {
		return errors.New("'server.timeout.{read|write}' value is not a valid time unit")
	}

	if appHTTPReadTimeout, err = time.ParseDuration(readTimeout); err != nil {
		return fmt.Errorf("'server.timeout.read': %s", err)
	}

	if appHTTPWriteTimeout, err = time.ParseDuration(writeTimeout); err != nil {
		return fmt.Errorf("'server.timeout.write': %s", err)
	}

	maxHdrBytesStr := cfg.StringDefault("server.max_header_bytes", "1mb")
	if maxHdrBytes, er := ess.StrToBytes(maxHdrBytesStr); er == nil {
		appHTTPMaxHdrBytes = int(maxHdrBytes)
	} else {
		return errors.New("'server.max_header_bytes' value is not a valid size unit")
	}

	appIsLetsEncrypt = cfg.BoolDefault("server.ssl.lets_encrypt.enable", false)
	appSSLCert = cfg.StringDefault("server.ssl.cert", "")
	appSSLKey = cfg.StringDefault("server.ssl.key", "")
	if err = checkSSLConfigValues(AppIsSSLEnabled(), appIsLetsEncrypt, appSSLCert, appSSLKey); err != nil {
		return err
	}

	if err = initAutoCertManager(cfg); err != nil {
		return err
	}

	multipartMemoryStr := cfg.StringDefault("request.multipart_size", "32mb")
	if appMultipartMaxMemory, err = ess.StrToBytes(multipartMemoryStr); err != nil {
		return errors.New("'request.multipart_size' value is not a valid size unit")
	}

	return nil
}

func initLogs(logsDir string, appCfg *config.Config) error {
	logCfg, found := appCfg.GetSubConfig("log")
	if !found {
		log.Debug("Section 'log {...}' configuration not exists, move on.")
		return nil
	}

	receiver := logCfg.StringDefault("receiver", "")

	if strings.EqualFold(receiver, "file") {
		file := logCfg.StringDefault("file", "")
		if ess.IsStrEmpty(file) {
			logCfg.SetString("file", filepath.Join(logsDir, getBinaryFileName()+".log"))
		} else if !filepath.IsAbs(file) {
			logCfg.SetString("file", filepath.Join(logsDir, file))
		}
	}

	logger, err := log.Newc(logCfg)
	if err != nil {
		return err
	}

	log.SetOutput(logger)

	return nil
}
