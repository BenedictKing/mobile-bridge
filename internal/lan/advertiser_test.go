package lan

import (
	"context"
	"errors"
	"net"
	"testing"
)

type fakeResponder struct{ stopped int }

func (f *fakeResponder) Shutdown() { f.stopped++ }

func validSpec() AdvertiseSpec {
	return AdvertiseSpec{Enabled: true, Service: ServiceType, Instance: "Office Mac",
		DeviceID: "0123456789abcdef0123456789abcdef", Port: 8787}
}

func TestValidateSpec(t *testing.T) {
	if err := ValidateSpec(AdvertiseSpec{}); err != nil {
		t.Fatalf("disabled spec: %v", err)
	}
	for _, change := range []func(*AdvertiseSpec){
		func(s *AdvertiseSpec) { s.Service = "_other._tcp" },
		func(s *AdvertiseSpec) { s.DeviceID = "not-a-device" },
		func(s *AdvertiseSpec) { s.Port = 0 },
	} {
		spec := validSpec()
		change(&spec)
		if err := ValidateSpec(spec); err == nil {
			t.Fatalf("expected invalid spec: %+v", spec)
		}
	}
}

func TestInstanceName(t *testing.T) {
	if got := InstanceName(" \nOffice\u0000 Mac "); got != "Office Mac" {
		t.Fatalf("unexpected instance %q", got)
	}
	if got := InstanceName(" \n"); got != "Mobile Bridge" {
		t.Fatalf("unexpected fallback %q", got)
	}
	if got := InstanceName("电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑电脑"); len(got) > 63 {
		t.Fatalf("instance is too long: %d", len(got))
	}
}

func TestPublishableInterfaces(t *testing.T) {
	interfaces := []net.Interface{
		{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagBroadcast},
		{Index: 2, Name: "lo0", Flags: net.FlagUp | net.FlagLoopback},
		{Index: 3, Name: "docker0", Flags: net.FlagUp | net.FlagBroadcast},
		{Index: 4, Name: "en1", Flags: net.FlagBroadcast},
		{Index: 5, Name: "tun0", Flags: net.FlagUp | net.FlagPointToPoint},
	}
	got := PublishableInterfaces(interfaces)
	if len(got) != 1 || got[0].Name != "en0" {
		t.Fatalf("unexpected interfaces: %+v", got)
	}
}

func TestMDNSAdvertiserLifecycle(t *testing.T) {
	original := registerService
	defer func() { registerService = original }()
	var specs []AdvertiseSpec
	fake := &fakeResponder{}
	registerService = func(spec AdvertiseSpec) (responder, error) {
		specs = append(specs, spec)
		return fake, nil
	}
	advertiser := NewMDNSAdvertiser()
	ctx, cancel := context.WithCancel(context.Background())
	if err := advertiser.Start(ctx, validSpec()); err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].DeviceID != validSpec().DeviceID {
		t.Fatalf("unexpected registration: %+v", specs)
	}
	if err := advertiser.Update(validSpec()); err != nil {
		t.Fatal(err)
	}
	if fake.stopped != 1 {
		t.Fatalf("update did not withdraw old registration: %d", fake.stopped)
	}
	if err := advertiser.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := advertiser.Stop(); err != nil {
		t.Fatal(err)
	}
	if fake.stopped != 2 {
		t.Fatalf("stop was not idempotent: %d", fake.stopped)
	}
	cancel()
}

func TestMDNSAdvertiserFailureDoesNotRetainState(t *testing.T) {
	original := registerService
	defer func() { registerService = original }()
	registerService = func(AdvertiseSpec) (responder, error) { return nil, errors.New("unavailable") }
	advertiser := NewMDNSAdvertiser()
	if err := advertiser.Start(context.Background(), validSpec()); err == nil {
		t.Fatal("expected registration error")
	}
	registerService = func(AdvertiseSpec) (responder, error) { return &fakeResponder{}, nil }
	if err := advertiser.Start(context.Background(), validSpec()); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if err := advertiser.Stop(); err != nil {
		t.Fatal(err)
	}
}
