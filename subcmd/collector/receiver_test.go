// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package collector

import (
	"testing"

	"go.opentelemetry.io/collector/receiver/otlpreceiver"
)

func TestOtlpFactory_CreateDefaultConfig(t *testing.T) {
	factory := otlpreceiver.NewFactory()
	for _, tc := range []struct {
		name             string
		collectorAddress string
		wantEndpoint     string
		wantTransport    string
	}{
		{
			name:             "tcp",
			collectorAddress: "localhost:4317",
			wantEndpoint:     "localhost:4317",
			wantTransport:    "tcp",
		},
		{
			name:             "unix",
			collectorAddress: "unix:///tmp/siso.sock",
			wantEndpoint:     "/tmp/siso.sock",
			wantTransport:    "unix",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &otlpFactory{
				Factory:          factory,
				collectorAddress: tc.collectorAddress,
			}
			cfg := f.CreateDefaultConfig().(*otlpreceiver.Config)

			grpcCfg := cfg.Protocols.GRPC.GetOrInsertDefault()

			if got := grpcCfg.NetAddr.Endpoint; got != tc.wantEndpoint {
				t.Errorf("Endpoint = %q; want %q", got, tc.wantEndpoint)
			}
			if got := grpcCfg.NetAddr.Transport; string(got) != tc.wantTransport {
				t.Errorf("Transport = %q; want %q", got, tc.wantTransport)
			}
		})
	}
}
