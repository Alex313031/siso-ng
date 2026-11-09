// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

import { LitElement, html } from 'lit';
import {
  formatIntervalMetricHuman,
  trimOutputPrefix,
} from './step-transforms.js';
import { Task, initialState } from '@lit/task';

// TODO: make these configurable; hardcoded to match the SSR recall form for now.
const RECALL_PROJECT = 'rbe-chrome-untrusted';
const RECALL_REAPI_INSTANCE = 'default_instance';

export class SisoStepDetails extends LitElement {
  static properties = {
    endpoint: { type: String },
  };

  createRenderRoot() {
    // Render in Light DOM so global styles (style.css, theme.css, Material Symbols)
    // and <clipboard-copy> document ID lookups apply seamlessly.
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
          <p style="color: var(--md-sys-color-on-surface-variant); margin: 0;">Loading build step...</p>
        </div>
      `,
      error: (e) => html`
        <div class="surface build-status failure">
          <md-icon>cancel</md-icon>
          <span>Failed to load build step: ${e.message}</span>
        </div>
      `,
      complete: (data) => this.renderStep(data),
    });
  }

  renderStep(data) {
    const step = data?.step;
    if (!step) {
      return html``;
    }

    const outdirRel = data?.outdirRel || '';
    const outputs = step.outputs || [];
    const primaryOutput = outputs[0] || '';
    const trimmedOutput = trimOutputPrefix(primaryOutput, outdirRel);
    const stepId = step.step_id || '';

    return html`
      <div class="step-page surface">
        <h2>${trimmedOutput}</h2>
        <ul class="step-details">
          <li>
            <md-icon>category</md-icon>
            <span>Used ninja action <code>${step.action || ''}</code></span>
          </li>
          <li>
            <md-icon>description</md-icon>
            ${step.rule
              ? html`<span>Applied siso rule <code>${step.rule}</code></span>`
              : html`<span>No siso rule applied</span>`}
          </li>
          <li>
            <md-icon>target</md-icon>
            <span>Part of GN target <code id="step-${stepId}-gntarget">${step.gn_target || ''}</code></span>
            <clipboard-copy for="step-${stepId}-gntarget">
              <md-icon-button>
                <md-icon>content_copy</md-icon>
              </md-icon-button>
            </clipboard-copy>
          </li>
          <li>
            <md-icon>timer</md-icon>
            Ran in ${formatIntervalMetricHuman(step.duration_nanos || 0)}
          </li>
          <li class="remote-indicator">
            ${step.is_local
              ? html`<md-icon>cloud_off</md-icon> Local Execution`
              : !step.cached && !step.no_exec
                ? html`<md-icon class="icon-cache-miss">cloud</md-icon> Remote Execution`
                : step.cached
                  ? html`<md-icon class="icon-cache-hit">cloud_done</md-icon> Cache Hit`
                  : ''}
          </li>
          ${step.digest
            ? html`
                <li>
                  <md-icon>cloud_circle</md-icon>
                  <span>
                    Digest:
                    <code id="step-${stepId}-digest">${step.digest}</code>
                  </span>
                  <clipboard-copy for="step-${stepId}-digest">
                    <md-icon-button>
                      <md-icon>content_copy</md-icon>
                    </md-icon-button>
                  </clipboard-copy>
                  <md-outlined-button @click=${() => this.querySelector('#recall-dialog').show()}>
                    Recall...
                  </md-outlined-button>
                  <md-dialog id="recall-dialog">
                    <div slot="headline">Recall step</div>
                    <form slot="content" id="recall-dialog-form" method="dialog">
                      <p>You can perform this recall with:</p>
                      <div class="code-block">
                        <pre id="recall-${stepId}"><code>siso recall -project="${RECALL_PROJECT}" -reapi_instance="${RECALL_REAPI_INSTANCE}" &lt;output_directory&gt; ${step.digest}</code></pre>
                        <clipboard-copy for="recall-${stepId}">
                          <md-icon-button>
                            <md-icon>content_copy</md-icon>
                          </md-icon-button>
                        </clipboard-copy>
                      </div>
                      <p>Native recall in this webui is still TODO.</p>
                    </form>
                    <div slot="actions">
                      <md-text-button form="recall-dialog-form">Ok</md-text-button>
                    </div>
                  </md-dialog>
                </li>
              `
            : ''}
          <li>
            <md-icon>input_circle</md-icon>
            ${step.inputs || 0} input(s)
          </li>
          <li>
            <md-icon>output_circle</md-icon>
            ${outputs.length} output(s)
          </li>
        </ul>
        ${outputs.length > 1
          ? html`
              <details class="step-outputs-details">
                <summary>Outputs (${outputs.length})</summary>
                <ul class="step-outputs">
                  ${outputs.map(
                    (out) => html`<li><code>${trimOutputPrefix(out, outdirRel)}</code></li>`,
                  )}
                </ul>
              </details>
            `
          : ''}
        <details>
          <summary>Raw metrics</summary>
          <dl class="step-raw">
            ${Object.keys(step)
              .sort()
              .map((key) => {
                const val = step[key];
                const formattedVal =
                  val !== null && typeof val === 'object'
                    ? JSON.stringify(val)
                    : String(val);
                return html`
                  <dt>${key}</dt>
                  <dd>
                    <span id="step-${stepId}-${key}-value">${formattedVal}</span>
                    <clipboard-copy for="step-${stepId}-${key}-value">
                      <md-icon-button>
                        <md-icon>content_copy</md-icon>
                      </md-icon-button>
                    </clipboard-copy>
                  </dd>
                `;
              })}
          </dl>
        </details>
      </div>
    `;
  }
}

customElements.define('siso-step-details', SisoStepDetails);
