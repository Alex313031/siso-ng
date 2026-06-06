// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package collector

import (
	"flag"
	"testing"
)

func TestSetFlags(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		args                 []string
		env                  map[string]string
		wantProjectID        string
		wantCollectorAddress string
	}{
		{
			name:                 "default",
			args:                 []string{},
			wantProjectID:        "",
			wantCollectorAddress: "",
		},
		{
			name:                 "flags",
			args:                 []string{"-project", "my-project", "-collector_address", "localhost:4317"},
			wantProjectID:        "my-project",
			wantCollectorAddress: "localhost:4317",
		},
		{
			name: "env",
			args: []string{},
			env: map[string]string{
				"SISO_PROJECT":           "env-project",
				"SISO_COLLECTOR_ADDRESS": "env-address",
			},
			wantProjectID:        "env-project",
			wantCollectorAddress: "env-address",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c := &Command{}
			flagSet := flag.NewFlagSet("collector", flag.ContinueOnError)
			c.SetFlags(flagSet)
			err := flagSet.Parse(tc.args)
			if err != nil {
				t.Fatalf("flag parse %v; want nil err", err)
			}
			if c.projectID != tc.wantProjectID {
				t.Errorf("projectID = %q; want %q", c.projectID, tc.wantProjectID)
			}
			if c.collectorAddress != tc.wantCollectorAddress {
				t.Errorf("collectorAddress = %q; want %q", c.collectorAddress, tc.wantCollectorAddress)
			}
		})
	}
}
