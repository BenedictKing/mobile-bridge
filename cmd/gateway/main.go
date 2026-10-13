// Command gateway is the Go gateway (plan doc P3). It serves the embedded
// web assets and the HTTP API pipeline with business routes stubbed; the
// conformance/http golden is the shared acceptance gate with the Python
// gateway.
//
// Environment:
//
//	GATEWAY_PORT          listen port (default 8790)
//	GATEWAY_AUTH          JSON: {"username","salt","hash","iterations"} (pbkdf2 record)
//	GATEWAY_INSTANCE_ID   deterministic instance id for tests
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	gateway "github.com/BenedictKing/mobile-bridge"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/httpd"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
)

func main() {
	port := 8790
	if value := os.Getenv("GATEWAY_PORT"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			log.Fatalf("GATEWAY_PORT: %v", err)
		}
		port = parsed
	}
	config := auth.Config{Mode: "password", Username: "admin", Iterations: 600000}
	if value := os.Getenv("GATEWAY_AUTH"); value != "" {
		var record struct {
			Mode       string `json:"mode"`
			Username   string `json:"username"`
			Salt       string `json:"salt"`
			Hash       string `json:"hash"`
			Iterations int    `json:"iterations"`
		}
		if err := json.Unmarshal([]byte(value), &record); err != nil {
			log.Fatalf("GATEWAY_AUTH: %v", err)
		}
		if record.Mode != "" {
			config.Mode = record.Mode
		}
		config.Username = record.Username
		config.Salt = record.Salt
		config.Hash = record.Hash
		config.Iterations = record.Iterations
	}
	instanceID := ""
	if value := os.Getenv("GATEWAY_INSTANCE_ID"); value != "" {
		instanceID = value
	} else {
		buffer := make([]byte, 16)
		if _, err := rand.Read(buffer); err != nil {
			log.Fatal(err)
		}
		instanceID = hex.EncodeToString(buffer)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	server := httpd.New(auth.NewAuth(config), gateway.WebFS, port, instanceID)
	dataDir := os.Getenv("GATEWAY_DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Join(".local")
	}
	server.Uploads = uploads.New(dataDir)
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			codexHome = filepath.Join(home, ".codex")
		}
	}
	if codexHome != "" && os.Getenv("GATEWAY_DISABLE_STORE") != "1" {
		server.Store = store.NewSessionStore(codexHome)
	}
	if codexHome != "" && os.Getenv("GATEWAY_DISABLE_IPC") != "1" {
		server.Codex = codex.New(codexHome, os.Getenv("GATEWAY_IPC_PATH"))
	}
	log.Printf("gateway listening on http://%s", listener.Addr())
	log.Fatal(http.Serve(listener, server))
}
