package main

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/labstack/echo/v4"

	httpserver "github.com/zenocy/zeno-v2/internal/http"
)

// uiFS holds the built UI. The placeholder file (`ui/dist/.gitkeep`) keeps
// the embed valid even when the UI hasn't been built. In production the
// Dockerfile runs `npm run build` before `go build`, so dist/ contains real
// assets.
//
//go:embed all:ui-dist
var uiFS embed.FS

func mountStaticUI(srv *httpserver.Server) {
	sub, err := fs.Sub(uiFS, "ui-dist")
	if err != nil {
		// Failure here means the embed itself is broken — surface loudly.
		panic("ui-dist embed: " + err.Error())
	}
	srv.Echo.GET("/*", echo.WrapHandler(http.FileServer(http.FS(sub))))
}
