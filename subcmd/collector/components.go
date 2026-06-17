// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package collector

import (
	googlecloudexporter "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/googlecloudexporter"
	healthcheckv2extension "github.com/open-telemetry/opentelemetry-collector-contrib/extension/healthcheckv2extension"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/receiver"
	otlpreceiver "go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"
	"google.golang.org/api/option"

	"go.chromium.org/build/siso/auth/cred"
)

type componentsConfig struct {
	credential       cred.Cred
	projectID        string
	collectorAddress string
	insecure         bool
}

func components(cfg componentsConfig) (otelcol.Factories, error) {
	var err error
	factories := otelcol.Factories{
		Telemetry: otelconftelemetry.NewFactory(),
	}

	factories.Extensions, err = otelcol.MakeFactoryMap[extension.Factory](
		healthcheckv2extension.NewFactory(),
	)
	if err != nil {
		return otelcol.Factories{}, err
	}
	factories.ExtensionModules = make(map[component.Type]string, len(factories.Extensions))
	factories.ExtensionModules[healthcheckv2extension.NewFactory().Type()] = "github.com/open-telemetry/opentelemetry-collector-contrib/extension/healthcheckv2extension"

	receiverFactory := &otlpFactory{
		Factory:          otlpreceiver.NewFactory(),
		collectorAddress: cfg.collectorAddress,
	}
	factories.Receivers, err = otelcol.MakeFactoryMap[receiver.Factory](
		receiverFactory,
	)
	if err != nil {
		return otelcol.Factories{}, err
	}
	factories.ReceiverModules = make(map[component.Type]string, len(factories.Receivers))
	factories.ReceiverModules[otlpreceiver.NewFactory().Type()] = "go.opentelemetry.io/collector/receiver/otlpreceiver"
	exporterFactory := &gceFactory{
		Factory:    googlecloudexporter.NewFactory(),
		credential: cfg.credential,
		projectID:  cfg.projectID,
		insecure:   cfg.insecure,
	}
	factories.Exporters, err = otelcol.MakeFactoryMap[exporter.Factory](
		exporterFactory,
	)
	if err != nil {
		return otelcol.Factories{}, err
	}
	factories.ExporterModules = make(map[component.Type]string, len(factories.Exporters))
	factories.ExporterModules[googlecloudexporter.NewFactory().Type()] = "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/googlecloudexporter"

	factories.Connectors, err = otelcol.MakeFactoryMap[connector.Factory]()
	if err != nil {
		return otelcol.Factories{}, err
	}
	factories.ConnectorModules = make(map[component.Type]string, len(factories.Connectors))

	return factories, nil
}

type gceFactory struct {
	exporter.Factory
	credential cred.Cred
	projectID  string
	insecure   bool
}

func (f gceFactory) CreateDefaultConfig() component.Config {
	config := f.Factory.CreateDefaultConfig().(*googlecloudexporter.Config)
	config.ProjectID = f.projectID
	config.TraceConfig.ClientConfig.GetClientOptions = f.clientOptions
	config.LogConfig.ClientConfig.GetClientOptions = f.clientOptions
	config.MetricConfig.ClientConfig.GetClientOptions = f.clientOptions
	return config
}

func (f gceFactory) clientOptions() []option.ClientOption {
	opts := f.credential.ClientOptions()
	if f.insecure {
		opts = append(opts, option.WithoutAuthentication())
	}
	return opts
}
