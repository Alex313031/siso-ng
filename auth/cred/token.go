// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cred

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

func fromTokenString(ctx context.Context, src, token string) (*oauth2.Token, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://oauth2.googleapis.com/tokeninfo", strings.NewReader("access_token="+token))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get tokeninfo: %w", err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to get tokeninfo body: %w", err)
	}
	type tokJSON struct {
		Exp   string `json:"exp"`
		Email string `json:"email"`

		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	var tok tokJSON
	err = json.Unmarshal(buf, &tok)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tokeninfo %q: %w", string(buf), err)
	}
	if tok.Error != "" {
		return nil, fmt.Errorf("token error: %s %s", tok.Error, tok.ErrorDescription)
	}
	exp, err := strconv.ParseInt(tok.Exp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse exp %q in %q (token:%q): %w", tok.Exp, string(buf), token, err)
	}
	t := &oauth2.Token{
		AccessToken: token,
		Expiry:      time.Unix(exp, 0),
	}
	t = t.WithExtra(map[string]any{
		"x-token-source": src,
		"x-token-email":  tok.Email,
	})
	return t, nil
}
