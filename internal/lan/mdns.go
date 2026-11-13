package lan

import "github.com/grandcat/zeroconf"

type responder interface {
	Shutdown()
}

var registerService = func(spec AdvertiseSpec) (responder, error) {
	return zeroconf.Register(spec.Instance, spec.Service, "local.", spec.Port,
		[]string{"v=1", "id=" + spec.DeviceID}, spec.Interfaces)
}
