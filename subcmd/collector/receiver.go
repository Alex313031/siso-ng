// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package collector

import (
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
)

type otlpFactory struct {
	receiver.Factory
	collectorAddress string
}

func (f *otlpFactory) CreateDefaultConfig() component.Config {
	cfg := f.Factory.CreateDefaultConfig().(*otlpreceiver.Config)

	grpcCfg := cfg.Protocols.GRPC.GetOrInsertDefault()
	switch {
	case strings.HasPrefix(f.collectorAddress, "unix:///"):
		// Submitting unix:/// path will result in error. It needs to be trimmed first.
		socketPath := strings.TrimPrefix(f.collectorAddress, "unix://")
		grpcCfg.NetAddr.Endpoint = socketPath
		grpcCfg.NetAddr.Transport = "unix"
	default:
		grpcCfg.NetAddr.Endpoint = f.collectorAddress
		grpcCfg.NetAddr.Transport = "tcp"
	}
	cfg.Protocols.GRPC = configoptional.Default(*grpcCfg)
	cfg.Protocols.HTTP = configoptional.Optional[otlpreceiver.HTTPConfig]{}

	return cfg
}
