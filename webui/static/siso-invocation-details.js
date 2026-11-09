// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

import { LitElement, html } from 'lit';
import { formatIntervalMetricHuman } from './step-transforms.js';
import { Task, initialState } from '@lit/task';

const BYTE_UNITS = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB'];

function formatBytes(b) {
  if (!b || b <= 0) {
    return 'N/A';
  }
  const unit = 1024;
  if (b < unit) {
    return `${b} B`;
  }
  let v = b / unit;
  let i = 0;
  while (v >= unit && i < BYTE_UNITS.length - 1) {
    v /= unit;
    i++;
  }
  return `${v.toFixed(1)} ${BYTE_UNITS[i]}`;
}

export class SisoInvocationDetails extends LitElement {
  static properties = {
    endpoint: { type: String },
  };

  createRenderRoot() {
    // Render in Light DOM so that global styles apply.
    return this;
  }

  constructor() {
    super();
    this.endpoint = '';
    this._task = new Task(this, {
      task: async ([endpoint], { signal }) => {
        if (!endpoint) {
          return initialState;
        }
        const response = await fetch(endpoint, { signal });
        if (!response.ok) {
          throw new Error(`HTTP error! status: ${response.status}`);
        }
        return response.json();
      },
      args: () => [this.endpoint],
    });
  }

  render() {
    return this._task.render({
      pending: () => html`
        <div class="surface" style="padding: 2rem; text-align: center;">
          <md-linear-progress indeterminate style="width: 100%; max-width: 360px; margin: 0 auto 16px;"></md-linear-progress>
          <p style="color: var(--md-sys-color-on-surface-variant); margin: 0;">Loading invocation details...</p>
        </div>
      `,
      error: (e) => html`
        <div class="surface build-status failure">
          <md-icon>cancel</md-icon>
          <span>Failed to load invocation details: ${e.message}</span>
        </div>
      `,
      complete: (data) => this.renderDetails(data),
    });
  }

  renderDetails(data) {
    if (!data) {
      return html``;
    }

    const info = data.info;

    let statusIcon = 'help';
    let statusText = 'Build Status Unknown';
    if (data.status === 'success') {
      statusIcon = 'check_circle';
      statusText = 'Build Succeeded';
    } else if (data.status === 'failure') {
      statusIcon = 'cancel';
      statusText = `Build Failed (${data.failedSteps || 0} steps failed)`;
    }

    const started = data.started;

    return html`
      <div class="step-page surface">
        <h2>Invocation Details</h2>
        <ul class="step-details">
          <li>
            <md-icon>${statusIcon}</md-icon>
            <span>${statusText}</span>
          </li>
          <li>
            <md-icon>fingerprint</md-icon>
            <span>Build ID: <code>${data.buildId || ''}</code></span>
          </li>
          <li>
            <md-icon>timer</md-icon>
            <span>Build Duration: ${formatIntervalMetricHuman(data.buildDurationNanos || 0)}</span>
          </li>
          ${started ? html`
            <li>
              <md-icon>schedule</md-icon>
              <span>Started at: ${started.time}${started.inferred ? html` <abbr title="Inferred start time (file modification time minus duration)">*</abbr>` : ''}</span>
            </li>
          ` : ''}
          ${info?.siso_version ? html`
            <li>
              <md-icon>info</md-icon>
              <span>Siso Version: <code>${info.siso_version}</code></span>
            </li>
          ` : ''}
          ${info?.targets && info.targets.length > 0 ? html`
            <li>
              <md-icon>target</md-icon>
              <span>Targets: <code>${info.targets.join(', ')}</code></span>
            </li>
          ` : ''}
          <li>
            <md-icon>list</md-icon>
            <span>Total Steps: ${data.totalSteps ?? 0}</span>
          </li>
        </ul>

        ${info?.machine?.cpu?.brand ? html`
          <h3>Machine & Environment</h3>
          <ul class="step-details">
            <li>
              <md-icon>computer</md-icon>
              <span>Platform: ${info.machine.platform?.os || ''} / ${info.machine.platform?.architecture || ''}${info.machine.platform?.os_version ? ` (${info.machine.platform.os_version})` : ''}</span>
            </li>
            <li>
              <md-icon>memory</md-icon>
              <span>CPU: ${info.machine.cpu.brand} (${info.machine.cpu.logical_cores} logical / ${info.machine.cpu.physical_cores} physical cores)</span>
            </li>
            ${info.machine.memory?.total ? html`
              <li>
                <md-icon>memory_alt</md-icon>
                <span>Total Memory: ${formatBytes(info.machine.memory.total)}</span>
              </li>
            ` : ''}
          </ul>
        ` : ''}

        ${info?.enabled_experiments && info.enabled_experiments.length > 0 ? html`
          <h3>Enabled Experiments</h3>
          <ul class="step-details">
            <li>
              <md-icon>science</md-icon>
              <span><code>${info.enabled_experiments.join(', ')}</code></span>
            </li>
          </ul>
        ` : ''}

        ${info?.metrics_labels && Object.keys(info.metrics_labels).length > 0 ? html`
          <h3>Metrics Labels</h3>
          <table>
            <thead>
              <tr>
                <th>Key</th>
                <th>Value</th>
              </tr>
            </thead>
            <tbody>
              ${Object.keys(info.metrics_labels).sort().map((k) => html`
                <tr>
                  <td><code>${k}</code></td>
                  <td><code>${info.metrics_labels[k]}</code></td>
                </tr>
              `)}
            </tbody>
          </table>
        ` : ''}
      </div>
    `;
  }
}

customElements.define('siso-invocation-details', SisoInvocationDetails);
