// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

import { LitElement, html } from 'lit';
import {
  computeAggregates,
  formatIntervalMetricTimestamp,
} from './step-transforms.js';
import { fetchSteps } from './steps-store.js';
import { Task, TaskStatus, initialState } from '@lit/task';

export class SisoAggregatesTable extends LitElement {
  static properties = {
    buildId: { type: String, attribute: 'build-id' },
    endpoint: { type: String },
    baseUrl: { type: String, attribute: 'base-url' },
    sortBy: { state: true },
    sortDsc: { state: true },
  };

  createRenderRoot() {
    // Render in Light DOM so that global styles (style.css, theme.css, Material Symbols, fonts) apply seamlessly.
    return this;
  }

  constructor() {
    super();
    this.buildId = '';
    this.endpoint = '';
    this.baseUrl = '';
    this.sortBy = 'utime';
    this.sortDsc = true;
    this._task = new Task(this, {
      task: async ([buildId, endpoint], { signal }) => {
        if (!buildId) {
          return initialState;
        }
        return fetchSteps(buildId, endpoint, { signal });
      },
      args: () => [this.buildId, this.endpoint],
    });
  }

  /**
   * Step metrics data from the most recent successful fetch, or null.
   */
  get _data() {
    return this._task.status === TaskStatus.COMPLETE ? this._task.value : null;
  }

  toggleSort(column) {
    if (this.sortBy === column) {
      this.sortDsc = !this.sortDsc;
    } else {
      this.sortBy = column;
      this.sortDsc = column === 'name' ? false : true;
    }
  }

  renderSortIcon(column) {
    if (this.sortBy !== column) {
      return '';
    }
    return this.sortDsc
      ? html`<md-icon slot="icon">arrow_downward</md-icon>`
      : html`<md-icon slot="icon">arrow_upward</md-icon>`;
  }

  get sortedAggregates() {
    if (!this._data || !this._data.steps) {
      return [];
    }
    const list = computeAggregates(this._data.steps);
    list.sort((a, b) => {
      let valA, valB;
      switch (this.sortBy) {
        case 'name':
          valA = a.aggregateBy;
          valB = b.aggregateBy;
          return this.sortDsc ? valB.localeCompare(valA) : valA.localeCompare(valB);
        case 'count':
          valA = a.count;
          valB = b.count;
          break;
        case 'duration':
          valA = a.totalDuration;
          valB = b.totalDuration;
          break;
        case 'weightedDuration':
          valA = a.totalWeightedDuration;
          valB = b.totalWeightedDuration;
          break;
        case 'utime':
        default:
          valA = a.totalUtime;
          valB = b.totalUtime;
          break;
      }
      const diff = valA - valB;
      return this.sortDsc ? -diff : diff;
    });
    return list;
  }

  render() {
    return this._task.render({
      pending: () => html`
        <div class="surface build-status">
          <p style="color: var(--md-sys-color-on-surface-variant); margin: 0;">Loading aggregates...</p>
        </div>
      `,
      error: (e) => html`
        <div class="surface build-status failure">
          <md-icon>cancel</md-icon>
          <span>Failed to load aggregates: ${e.message}</span>
        </div>
      `,
      initial: () => this.renderTable(),
      complete: () => this.renderTable(),
    });
  }

  renderTable() {
    const aggregates = this.sortedAggregates;

    return html`
      <div class="data-table-container">
        <table>
          <thead>
            <tr>
              <th scope="col">
                <md-text-button
                  class="sort-header"
                  trailing-icon
                  @click=${() => this.toggleSort('name')}
                >
                  Aggregate By
                  ${this.renderSortIcon('name')}
                </md-text-button>
              </th>
              <th class="timestamp-column" scope="col">
                <md-text-button
                  class="sort-header"
                  trailing-icon
                  @click=${() => this.toggleSort('count')}
                >
                  Count
                  ${this.renderSortIcon('count')}
                </md-text-button>
              </th>
              <th class="timestamp-column" scope="col">
                <md-text-button
                  class="sort-header"
                  trailing-icon
                  @click=${() => this.toggleSort('utime')}
                >
                  Total Utime
                  ${this.renderSortIcon('utime')}
                </md-text-button>
              </th>
              <th class="timestamp-column" scope="col">
                <md-text-button
                  class="sort-header"
                  trailing-icon
                  @click=${() => this.toggleSort('duration')}
                >
                  Total Duration
                  ${this.renderSortIcon('duration')}
                </md-text-button>
              </th>
              <th class="timestamp-column" scope="col">
                <md-text-button
                  class="sort-header"
                  trailing-icon
                  @click=${() => this.toggleSort('weightedDuration')}
                >
                  Total Weighted Duration
                  ${this.renderSortIcon('weightedDuration')}
                </md-text-button>
              </th>
            </tr>
          </thead>
          <tbody>
            ${aggregates.map((row) => {
              const filterParam = row.isRule ? 'rule' : 'action';
              const link = this.baseUrl
                ? `${this.baseUrl}/steps_lit/?${filterParam}=${encodeURIComponent(row.aggregateBy)}`
                : null;
              return html`
                <tr>
                  <td>
                    ${link
                      ? html`<a href="${link}"><code>${row.aggregateBy}</code></a>`
                      : html`<code>${row.aggregateBy}</code>`}
                  </td>
                  <td class="timestamp">${row.count.toLocaleString()}</td>
                  <td class="timestamp">${formatIntervalMetricTimestamp(row.totalUtime)}</td>
                  <td class="timestamp">${formatIntervalMetricTimestamp(row.totalDuration)}</td>
                  <td class="timestamp">${formatIntervalMetricTimestamp(row.totalWeightedDuration)}</td>
                </tr>
              `;
            })}
          </tbody>
        </table>
      </div>
    `;
  }
}

customElements.define('siso-aggregates-table', SisoAggregatesTable);
