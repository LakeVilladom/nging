/*

   Copyright 2016 Wenhui Shen <www.webx.top>

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.

*/
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"strings"

	assetfs "github.com/elazarl/go-bindata-assetfs"
	"github.com/webx-top/echo"
	"github.com/webx-top/echo/engine"
	"github.com/webx-top/echo/engine/standard"
	"github.com/webx-top/echo/handler/mvc/events"
	"github.com/webx-top/echo/middleware"
	"github.com/webx-top/echo/middleware/bindata"
	"github.com/webx-top/echo/middleware/language"
	"github.com/webx-top/echo/middleware/render"
	"github.com/webx-top/echo/middleware/session"

	"github.com/admpub/letsencrypt"
	"github.com/admpub/log"
	"github.com/admpub/nging/application"
	"github.com/admpub/nging/application/library/config"
	"github.com/admpub/nging/application/library/modal"
)

var Version = `0.1.0 beta1`
var binData bool

func main() {
	config.DefaultCLIConfig.InitFlag()
	flag.Parse()
	if binData {
		Version += ` (bindata)`
	}
	config.SetVersion(Version)

	err := config.ParseConfig()
	if err != nil {
		if config.IsInstalled() {
			config.MustOK(err)
		} else {
			log.Error(err)
		}
	}

	if config.DefaultCLIConfig.OnlyRunServer() {
		return
	}
	config.DefaultCLIConfig.RunStartup()

	e := echo.New()
	e.SetDebug(true)
	e.Use(middleware.Log(), middleware.Recover())
	e.Use(func(h echo.Handler) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set(`Server`, `nging/`+Version)
			return h.Handle(c)
		}
	})

	// 注册静态资源文件
	if binData {
		e.Use(bindata.Static("/public/", &assetfs.AssetFS{
			Asset:     Asset,
			AssetDir:  AssetDir,
			AssetInfo: AssetInfo,
			Prefix:    "",
		}))
	} else {
		e.Use(middleware.Static(&middleware.StaticOptions{
			Root: "./public",
			Path: "/public/",
		}))
	}

	// 启用多语言支持
	e.Use(language.New(&config.DefaultConfig.Language).Middleware())

	// 启用session
	e.Use(session.Middleware(config.SessionOptions))

	renderOptions := &render.Config{
		TmplDir: `./template`,
		Engine:  `standard`,
		ParseStrings: map[string]string{
			`__PUBLIC__`: `/public`,
			`__ASSETS__`: `/public/assets`,
			`__TMPL__`:   `./template`,
		},
		Reload:     true,
		ErrorPages: config.DefaultConfig.Sys.ErrorPages,
	}
	renderOptions.ApplyTo(e)

	// 注册模板引擎
	if binData {
		manager := bindata.NewTmplManager(&assetfs.AssetFS{
			Asset:     Asset,
			AssetDir:  AssetDir,
			AssetInfo: AssetInfo,
			Prefix:    "template",
		})
		renderOptions.Renderer().SetManager(manager)
		modal.ReadConfigFile = func(file string) ([]byte, error) {
			file = strings.TrimPrefix(file, `./template`)
			return manager.GetTemplate(file)
		}
	}
	events.AddEvent(`clearCache`, func(next func(r bool), args ...interface{}) {
		renderOptions.Renderer().ClearCache()
		next(true)
	})

	application.Initialize(e)

	c := &engine.Config{
		Address: fmt.Sprintf(`:%v`, config.DefaultCLIConfig.Port),

		TLSCertFile: config.DefaultConfig.Sys.SSLCertFile,
		TLSKeyFile:  config.DefaultConfig.Sys.SSLKeyFile,
	}
	if len(config.DefaultConfig.Sys.SSLHosts) > 0 {
		var tlsManager letsencrypt.Manager
		tlsManager.SetHosts(config.DefaultConfig.Sys.SSLHosts)
		if err := tlsManager.CacheFile(config.DefaultConfig.Sys.SSLCacheFile); err != nil {
			panic(err.Error())
		}
		c.TLSConfig = &tls.Config{
			GetCertificate: tlsManager.GetCertificate,
		}
	}
	e.Run(standard.NewWithConfig(c))

}
