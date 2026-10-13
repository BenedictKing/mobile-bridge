package main

import (
	"embed"
	"github.com/wailsapp/wails/v3/pkg/application"
	"log"
)

//go:embed frontend
var assets embed.FS

type GatewayService struct{ gateway string }

func NewGatewayService() *GatewayService { return &GatewayService{} }

func (s *GatewayService) Info() map[string]any {
	return map[string]any{"gateway": resolveGateway(s.gateway), "dataDir": resolveDataDir()}
}

func (s *GatewayService) Snapshot() (map[string]any, error) {
	return runAction(s.gateway, "snapshot", nil)
}
func (s *GatewayService) Save(value map[string]any) (map[string]any, error) {
	return runAction(s.gateway, "save", value)
}
func (s *GatewayService) Start() (map[string]any, error) { return runAction(s.gateway, "start", nil) }
func (s *GatewayService) Stop() (map[string]any, error)  { return runAction(s.gateway, "stop", nil) }

func main() {
	service := NewGatewayService()
	app := application.New(application.Options{
		Name:        "Mobile Bridge",
		Description: "Codex 手机网关 Go 桌面控制器",
		Services:    []application.Service{application.NewService(service)},
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		Mac:         application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false},
	})
	app.Window.NewWithOptions(application.WebviewWindowOptions{Title: "Mobile Bridge", Width: 1100, Height: 760, URL: "/"})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
