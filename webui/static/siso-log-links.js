// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

import { LitElement, html } from 'lit';
import { openInPerfetto } from './main.js';

// Files the server serves at the outdir level, not per-build.
const OUTDIR_LEVEL_FILES = new Set(['.siso_config', '.siso_filegroups']);

/**
 * Lists an invocation's raw log files as links to the server's raw endpoint.
 *
 * Attributes:
 *   base-url: the invocation's base URL, e.g. /out/Default/builds/{rev}
 *   files: comma-separated list of log file names
 */
export class SisoLogLinks extends LitElement {
  static properties = {
    baseUrl: { type: String, attribute: 'base-url' },
    files: {
      converter: (value) => (value || '').split(',').map((s) => s.trim()).filter(Boolean),
    },
  };

  createRenderRoot() {
    // Render in Light DOM so that global styles apply.
    return this;
  }

  render() {
    const base = (this.baseUrl || '').replace(/\/+$/, '');
    return html`
      <ul class="step-details">
        ${(this.files || []).map((file) => {
          const rawUrl = `${base}/logs/${file}?raw=true`;
          return html`
            <li>
              <md-icon>description</md-icon>
              <a href=${rawUrl} target="_blank" rel="noopener noreferrer"><code>${file}</code></a>
              ${OUTDIR_LEVEL_FILES.has(file) ? html`<small>(outdir-level, not per-build)</small>` : ''}
              ${file === 'siso_trace.json' ? html`
                <button class="link-button" @click=${() => openInPerfetto(rawUrl)}>
                  <md-icon>speed</md-icon>
                  <span class="text">Open in Perfetto</span>
                </button>` : ''}
            </li>
          `;
        })}
      </ul>
    `;
  }
}

customElements.define('siso-log-links', SisoLogLinks);
