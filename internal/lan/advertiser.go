package lan

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

const ServiceType = "_mobile-bridge._tcp"

var deviceIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type AdvertiseSpec struct {
	Enabled    bool
	Service    string
	Instance   string
	DeviceID   string
	Port       int
	Interfaces []net.Interface
}

type Advertiser interface {
	Start(context.Context, AdvertiseSpec) error
	Update(AdvertiseSpec) error
	Stop() error
}

func ValidateSpec(spec AdvertiseSpec) error {
	if !spec.Enabled {
		return nil
	}
	if spec.Service != ServiceType {
		return errors.New("局域网发现服务类型无效")
	}
	if !deviceIDPattern.MatchString(spec.DeviceID) {
		return errors.New("局域网发现设备 ID 无效")
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return errors.New("局域网发现端口无效")
	}
	if InstanceName(spec.Instance) == "" {
		return errors.New("局域网发现实例名无效")
	}
	return nil
}

func InstanceName(value string) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value))
	if value == "" {
		return "Mobile Bridge"
	}
	for len(value) > 63 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value)
}

func PublishableInterfaces(interfaces []net.Interface) []net.Interface {
	result := make([]net.Interface, 0, len(interfaces))
	for _, iface := range interfaces {
		name := strings.ToLower(iface.Name)
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		virtual := false
		for _, prefix := range []string{"docker", "veth", "virbr", "br-", "utun", "awdl", "llw", "anpi", "tailscale", "vmnet", "vboxnet", "wsl", "zt"} {
			if strings.HasPrefix(name, prefix) {
				virtual = true
				break
			}
		}
		if !virtual {
			result = append(result, iface)
		}
	}
	return result
}

type MDNSAdvertiser struct {
	mu       sync.Mutex
	server   responder
	cancel   context.CancelFunc
	starting bool
}

func NewMDNSAdvertiser() *MDNSAdvertiser {
	return &MDNSAdvertiser{}
}

func (a *MDNSAdvertiser) Start(ctx context.Context, spec AdvertiseSpec) error {
	if err := ValidateSpec(spec); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.starting {
		return errors.New("局域网发现广播已启动")
	}
	if !spec.Enabled {
		return nil
	}
	spec.Instance = InstanceName(spec.Instance)
	server, err := registerService(spec)
	if err != nil {
		return err
	}
	child, cancel := context.WithCancel(ctx)
	a.server = server
	a.cancel = cancel
	a.starting = true
	go func() {
		<-child.Done()
		_ = a.Stop()
	}()
	return nil
}

func (a *MDNSAdvertiser) Update(spec AdvertiseSpec) error {
	if err := a.Stop(); err != nil {
		return err
	}
	return a.Start(context.Background(), spec)
}

func (a *MDNSAdvertiser) Stop() error {
	a.mu.Lock()
	server, cancel := a.server, a.cancel
	a.server, a.cancel, a.starting = nil, nil, false
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if server != nil {
		server.Shutdown()
	}
	return nil
}
