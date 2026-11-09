// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

import { LitElement, html } from 'lit';
import { stepQueryState } from './step-query-state.js';

export class SisoStepSidebar extends LitElement {
  static properties = {
    totalItems: { type: Number },
    actionCounts: { type: Array },
    ruleCounts: { type: Array },
  };

  constructor(queryState = stepQueryState) {
    super();
    this.queryState = queryState;
    this.totalItems = 0;
    this.actionCounts = [];
    this.ruleCounts = [];
    this._onQueryStateChange = () => this.requestUpdate();
    this._onStepsUpdated = (e) => this._handleStepsUpdated(e);
  }

  // Render in Light DOM so global CSS (#page-sidebar, .step-views, etc.) applies directly.
  createRenderRoot() {
    return this;
  }

  connectedCallback() {
    super.connectedCallback();
    (this.queryState || stepQueryState).addEventListener('change', this._onQueryStateChange);
    document.addEventListener('steps-updated', this._onStepsUpdated);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    (this.queryState || stepQueryState).removeEventListener('change', this._onQueryStateChange);
    document.removeEventListener('steps-updated', this._onStepsUpdated);
  }

  _handleStepsUpdated(e) {
    const { totalFilteredItems, actionCounts, ruleCounts } = e.detail || {};
    if (typeof totalFilteredItems === 'number') {
      this.totalItems = totalFilteredItems;
    }
    if (Array.isArray(actionCounts)) {
      this.actionCounts = actionCounts;
    }
    if (Array.isArray(ruleCounts)) {
      this.ruleCounts = ruleCounts;
    }
  }

  _setPage(page) {
    (this.queryState || stepQueryState).setPage(page, this.totalItems);
  }

  _onPageInputChange(e) {
    const val = parseInt(e.target.value, 10);
    this._setPage(isNaN(val) ? 0 : val);
    if (e.target) {
      e.target.value = String((this.queryState || stepQueryState).page);
    }
    this.requestUpdate();
  }

  _onSearchInput(e) {
    (this.queryState || stepQueryState).setSearchQuery(e.target.value);
  }

  _onClearSearch(e) {
    const input = e.target.closest('md-filled-text-field');
    if (input) {
      input.value = '';
    }
    (this.queryState || stepQueryState).setSearchQuery('');
  }

  renderViews() {
    const currentView = (this.queryState || stepQueryState).view || '';
    return html`
      <div class="step-views">
        <h3 id="view-label">View</h3>
        <input type="radio" id="view-all" name="view" value=""
            .checked=${currentView === ''}
            @change=${() => (this.queryState || stepQueryState).setView('')}>
        <label for="view-all">All<md-ripple></md-ripple></label>

        <md-outlined-button disabled>Running</md-outlined-button>
        <md-outlined-button disabled>Failed</md-outlined-button>

        <input type="radio" id="view-local-only" name="view" value="localOnly"
            .checked=${currentView === 'localOnly'}
            @change=${() => (this.queryState || stepQueryState).setView('localOnly')}>
        <label for="view-local-only">Local<md-ripple></md-ripple></label>

        <input type="radio" id="view-critical-path" name="view" value="criticalPath"
            .checked=${currentView === 'criticalPath'}
            @change=${() => (this.queryState || stepQueryState).setView('criticalPath')}>
        <label for="view-critical-path">Critical Path<md-ripple></md-ripple></label>
      </div>
    `;
  }

  renderPagination() {
    const pagination = (this.queryState || stepQueryState).getPagination(this.totalItems);
    const isFirstPage = pagination.pageIndex === 0;
    const isLastPage = pagination.pageIndex >= pagination.pageLast;

    return html`
      <div class="step-paginator">
        Showing ${pagination.itemFirstLogical}-${pagination.itemLastLogical} of ${pagination.totalItems}
      </div>
      <div class="page-controls">
        <md-outlined-text-field class="page-number" type="number" name="page" aria-label="Page"
            .value=${String(pagination.pageIndex)} min="0" max=${String(pagination.pageLast)}
            @change=${(e) => this._onPageInputChange(e)}></md-outlined-text-field>
        <md-icon-button ?disabled=${isFirstPage} @click=${() => this._setPage(0)}>
          <md-icon>first_page</md-icon>
        </md-icon-button>
        <md-icon-button ?disabled=${isFirstPage} @click=${() => this._setPage(pagination.pageIndex - 1)}>
          <md-icon>chevron_left</md-icon>
        </md-icon-button>
        <md-icon-button ?disabled=${isLastPage} @click=${() => this._setPage(pagination.pageIndex + 1)}>
          <md-icon>chevron_right</md-icon>
        </md-icon-button>
        <md-icon-button ?disabled=${isLastPage} @click=${() => this._setPage(pagination.pageLast)}>
          <md-icon>last_page</md-icon>
        </md-icon-button>
      </div>
    `;
  }

  renderSearch() {
    const query = (this.queryState || stepQueryState).searchQuery || '';
    return html`
      <md-filled-text-field name="q" type="search" .value=${query} placeholder="Search outputs"
          @input=${(e) => this._onSearchInput(e)}>
        <md-icon slot="leading-icon">search</md-icon>
        <md-icon-button class="clear-text-field" slot="trailing-icon"
            style="display: ${query ? 'flex' : 'none'}"
            @click=${(e) => this._onClearSearch(e)}>
          <md-icon>close</md-icon>
        </md-icon-button>
      </md-filled-text-field>
    `;
  }

  renderFilters() {
    const qs = this.queryState || stepQueryState;
    return html`
      <div class="filter-controls" id="step-filter-controls">
        ${this.renderSearch()}
        <div>
          <button type="button" class="menu-popover-button" popovertarget="action-filter-popover">
            <md-icon>filter_alt</md-icon>
            Ninja rules
            <md-ripple></md-ripple>
          </button>
          <div popover class="menu-popover filter-popover" id="action-filter-popover">
            <div class="menu-popover-content">
              <ul>
                ${this.actionCounts.map((item) => html`
                  <li>
                    <md-checkbox id="action-${item.Key}" name="action" .value=${item.Key}
                        .checked=${qs.actionFilters.has(item.Key)}
                        @change=${() => qs.toggleActionFilter(item.Key)}></md-checkbox>
                    <label for="action-${item.Key}">${item.Key} (${item.Count})</label>
                  </li>
                `)}
              </ul>
              <md-elevation></md-elevation>
            </div>
          </div>
        </div>
        <div>
          <button type="button" class="menu-popover-button" popovertarget="rule-filter-popover">
            <md-icon>filter_alt</md-icon>
            Siso configs
            <md-ripple></md-ripple>
          </button>
          <div popover class="menu-popover filter-popover" id="rule-filter-popover">
            <div class="menu-popover-content">
              <ul>
                ${this.ruleCounts.map((item) => html`
                  <li>
                    <md-checkbox id="rule-${item.Key}" name="rule" .value=${item.Key}
                        .checked=${qs.ruleFilters.has(item.Key)}
                        @change=${() => qs.toggleRuleFilter(item.Key)}></md-checkbox>
                    <label for="rule-${item.Key}">${item.Key} (${item.Count})</label>
                  </li>
                `)}
              </ul>
              <md-elevation></md-elevation>
            </div>
          </div>
        </div>
      </div>
    `;
  }

  render() {
    return html`
      ${this.renderViews()}
      ${this.renderPagination()}
      ${this.renderFilters()}
    `;
  }
}

customElements.define('siso-step-sidebar', SisoStepSidebar);
