// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

import { LitElement, html } from 'lit';
import {
  filterSteps,
  formatIntervalMetricHuman,
  formatIntervalMetricTimestamp,
  sortSteps,
  trimOutputPrefix,
} from './step-transforms.js';
import { stepQueryState } from './step-query-state.js';
import { fetchSteps } from './steps-store.js';
import { Task, TaskStatus, initialState } from '@lit/task';

export class SisoStepTable extends LitElement {
  static properties = {
    buildId: { type: String, attribute: 'build-id' },
    endpoint: { type: String },
    baseUrl: { type: String, attribute: 'base-url' },
  };

  createRenderRoot() {
    // Render in Light DOM so that global styles (style.css, theme.css, Material Symbols, fonts) apply seamlessly.
    return this;
  }

  constructor(queryState = stepQueryState) {
    super();
    this.buildId = '';
    this.endpoint = '';
    this.baseUrl = '';
    this._actionCounts = [];
    this._ruleCounts = [];
    this._countsData = null;
    this._task = new Task(this, {
      task: async ([buildId, endpoint], { signal }) => {
        if (!buildId) {
          return initialState;
        }
        return fetchSteps(buildId, endpoint, { signal });
      },
      args: () => [this.buildId, this.endpoint],
    });

    this.queryState = queryState;
    this._onQueryChange = () => this.requestUpdate();
  }

  connectedCallback() {
    super.connectedCallback();
    this.queryState.addEventListener('change', this._onQueryChange);
    this.queryState.initFromURL();
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.queryState.removeEventListener('change', this._onQueryChange);
  }

  /**
   * Step metrics data from the most recent successful fetch, or null.
   */
  get _data() {
    return this._task.status === TaskStatus.COMPLETE ? this._task.value : null;
  }

  get filteredSteps() {
    if (!this._data || !this._data.steps) {
      return [];
    }
    return filterSteps(this._data.steps, this.queryState);
  }

  get sortedSteps() {
    return sortSteps(this.filteredSteps, this.queryState);
  }

  /**
   * Renders the execution timeline SVG for a build step.
   * @param {!object} step
   * @return {!import('lit').TemplateResult}
   */
  renderTimeline(step) {
    const buildDuration = this._data?.buildDurationNanos || 1;
    const ready = step.ready_nanos || 0;
    const start = step.start_nanos || 0;
    const duration = step.duration_nanos || 0;
    const actionStart = step.action_start_nanos || 0;
    const actionEnd = step.action_end_nanos || 0;
    const run = step.run_nanos || 0;
    const execStart = step.exec_start_nanos;
    const execTime = step.exec_nanos || 0;

    const scale = (val) => (val / buildDuration) * 100;

    return html`
      <svg height="20" class="step-timeline-bar" width="100%">
        <rect
          x="${scale(ready)}%"
          y="0"
          width="${scale(start)}%"
          height="20"
          style="fill: rgb(50, 176, 73)"
        />
        <rect
          x="${scale(ready + start)}%"
          y="0"
          width="${scale(duration)}%"
          height="20"
          style="fill: rgb(48, 48, 48)"
        />
        <rect
          x="${scale(actionStart)}%"
          y="0"
          width="${scale(Math.max(0, actionEnd - actionStart))}%"
          height="20"
          style="fill: rgb(251, 255, 0)"
        />
        <rect
          x="${scale(actionStart)}%"
          y="0"
          width="${scale(run)}%"
          height="20"
          style="fill: rgb(223, 134, 30)"
        />
        ${execStart
          ? html`
              <rect
                x="${scale(execStart)}%"
                y="0"
                width="${scale(execTime)}%"
                height="20"
                style="fill: rgb(134, 39, 222)"
              />
            `
          : ''}
      </svg>
    `;
  }

  computeActionCounts() {
    if (!this._data?.steps) return [];
    const counts = new Map();
    for (const step of this._data.steps) {
      if (!step.action) continue;
      counts.set(step.action, (counts.get(step.action) || 0) + 1);
    }
    return Array.from(counts.entries())
      .filter(([_, count]) => count > 1) // Match SSR behavior (only actions with > 1 occurrence)
      .map(([Key, Count]) => ({ Key, Count }))
      .sort((a, b) => b.Count - a.Count || a.Key.localeCompare(b.Key));
  }

  computeRuleCounts() {
    if (!this._data?.steps) return [];
    const counts = new Map();
    for (const step of this._data.steps) {
      if (!step.rule) continue;
      counts.set(step.rule, (counts.get(step.rule) || 0) + 1);
    }
    return Array.from(counts.entries())
      .map(([Key, Count]) => ({ Key, Count }))
      .sort((a, b) => b.Count - a.Count || a.Key.localeCompare(b.Key));
  }

  updated(changedProperties) {
    super.updated(changedProperties);
    // Task-driven updates don't appear in changedProperties, so track the
    // data identity directly.
    if (this._data !== this._countsData) {
      this._countsData = this._data;
      this._actionCounts = this.computeActionCounts();
      this._ruleCounts = this.computeRuleCounts();
    }
    const sorted = this.sortedSteps;
    const pagination = this.queryState.getPagination(sorted.length);
    this.dispatchEvent(new CustomEvent('steps-updated', {
      bubbles: true,
      composed: true,
      detail: {
        totalFilteredItems: sorted.length,
        pagination,
        actionCounts: this._actionCounts,
        ruleCounts: this._ruleCounts,
      },
    }));
  }

  render() {
    return this._task.render({
      pending: () => html`
        <div class="surface" style="padding: 2rem; text-align: center;">
          <md-linear-progress indeterminate style="width: 100%; max-width: 360px; margin: 0 auto 16px;"></md-linear-progress>
          <p style="color: var(--md-sys-color-on-surface-variant); margin: 0;">Loading build steps...</p>
        </div>
      `,
      error: (e) => html`
        <div class="surface build-status failure">
          <md-icon>cancel</md-icon>
          <span>Failed to load build steps: ${e.message}</span>
        </div>
      `,
      initial: () => this.renderTable(),
      complete: () => this.renderTable(),
    });
  }

  renderTable() {
    const sorted = this.sortedSteps;
    const pagination = this.queryState.getPagination(sorted.length);
    const currentSubset = sorted.slice(pagination.itemsFirst, pagination.itemsLast);

    const isReadyAsc = this.queryState.sortBy === 'ready' && !this.queryState.sortDsc;
    const isReadyDsc = this.queryState.sortBy === 'ready' && this.queryState.sortDsc;
    const isDurationAsc = this.queryState.sortBy === 'duration' && !this.queryState.sortDsc;
    const isDurationDsc = this.queryState.sortBy === 'duration' && this.queryState.sortDsc;
    const isCompletionAsc = this.queryState.sortBy === 'completion' && !this.queryState.sortDsc;
    const isCompletionDsc = this.queryState.sortBy === 'completion' && this.queryState.sortDsc;
    const isCriticalPath = this.queryState.view === 'criticalPath';

    const outdirRel = this._data?.outdirRel || '';

    return html`
      <div class="data-table-container">
        <table>
          <thead>
            <tr>
              <th scope="col">
                Output
                <div class="subrow">Rule</div>
              </th>
              <th class="thin-column remote-indicator" scope="col">Remote</th>
              ${!isCriticalPath
                ? html`
                    <th class="timestamp-column" scope="col">
                      <md-text-button
                        class="sort-header"
                        trailing-icon
                        @click=${() => this.queryState.toggleSort('ready')}
                      >
                        Ready
                        ${isReadyAsc
                          ? html`<md-icon slot="icon">arrow_upward</md-icon>`
                          : ""}
                        ${isReadyDsc
                          ? html`<md-icon slot="icon">arrow_downward</md-icon>`
                          : ""}
                      </md-text-button>
                    </th>
                    <th class="timestamp-column" scope="col">
                      <md-text-button
                        class="sort-header"
                        trailing-icon
                        @click=${() => this.queryState.toggleSort('duration')}
                      >
                        Duration
                        ${isDurationAsc
                          ? html`<md-icon slot="icon">arrow_upward</md-icon>`
                          : ""}
                        ${isDurationDsc
                          ? html`<md-icon slot="icon">arrow_downward</md-icon>`
                          : ""}
                      </md-text-button>
                    </th>
                    <th class="timestamp-column" scope="col">
                      <md-text-button
                        class="sort-header"
                        trailing-icon
                        @click=${() => this.queryState.toggleSort('completion')}
                      >
                        Completion
                        ${isCompletionAsc
                          ? html`<md-icon slot="icon">arrow_upward</md-icon>`
                          : ""}
                        ${isCompletionDsc
                          ? html`<md-icon slot="icon">arrow_downward</md-icon>`
                          : ""}
                      </md-text-button>
                    </th>
                  `
                : html`
                    <th scope="col">Ready</th>
                    <th scope="col">Duration</th>
                    <th scope="col">Completion</th>
                  `}
              <th class="timeline-column" scope="col">Timeline</th>
            </tr>
          </thead>
          <tbody id="data-table-body">
            ${currentSubset.map((step) => {
              const output = step.outputs?.[0] || "";
              const trimmedOutput = trimOutputPrefix(output, outdirRel);
              const stepUrl = this.baseUrl
                ? `${this.baseUrl}/steps_lit/${encodeURIComponent(step.step_id || "")}/`
                : "#";
              const isLocal = step.is_local;
              const isCached = step.cached;
              const isNoExec = step.no_exec;

              return html`
                <tr>
                  <th scope="row">
                    <a href="${stepUrl}">${trimmedOutput}</a>
                    <div class="subrow">
                      <span class="step-rule">${step.rule || ""}</span>
                      <span class="step-action">${step.action || ""}</span>
                    </div>
                  </th>
                  <td class="remote-indicator">
                    ${isLocal
                      ? ""
                      : !isCached && !isNoExec
                        ? html`
                            <md-icon
                              class="icon-cache-miss"
                              title="Remote Execution"
                              >cloud</md-icon
                            >
                          `
                        : isCached
                          ? html`
                              <md-icon class="icon-cache-hit" title="Cache Hit"
                                >cloud_done</md-icon
                              >
                            `
                          : ""}
                  </td>
                  <td class="timestamp">
                    ${formatIntervalMetricTimestamp(step.ready_nanos || 0)}
                  </td>
                  <td class="timestamp">
                    ${formatIntervalMetricHuman(step.duration_nanos || 0)}
                  </td>
                  <td class="timestamp">
                    ${formatIntervalMetricTimestamp(
                      (step.ready_nanos || 0) + (step.duration_nanos || 0),
                    )}
                  </td>
                  <td>${this.renderTimeline(step)}</td>
                </tr>
              `;
            })}
          </tbody>
        </table>
      </div>
    `;
  }
}

// TODO: https://google.github.io/styleguide/jsguide.html#features-strings-use-single-quotes
customElements.define("siso-step-table", SisoStepTable);
