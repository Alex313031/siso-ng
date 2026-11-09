// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

/**
 * Singleton to manage syncing URL query parameters with the client-side
 * rendered step filtering state.
 */
export class StepQueryState extends EventTarget {
  constructor() {
    super();
    this.reset();
  }

  /**
   * Resets all query state fields to their default values.
   */
  reset() {
    this.searchQuery = '';
    this.view = '';
    this.actionFilters = new Set();
    this.ruleFilters = new Set();
    this.sortBy = 'ready';
    this.sortDsc = false;
    this.page = 0;
    this.itemsPerPage = 100;
  }

  /**
   * Initializes state from URL query parameters.
   * Any parameter missing from the URL is reset to its default.
   * @param {string} [search] Optional search query string; defaults to window.location.search.
   */
  initFromURL(search) {
    this.reset();
    try {
      const searchStr =
        search !== undefined
          ? search
          : typeof window !== 'undefined' && window.location
            ? window.location.search
            : '';
      const params = new URLSearchParams(searchStr);

      const q = params.get('q');
      if (q !== null) {
        this.searchQuery = q;
      }

      const view = params.get('view');
      if (view !== null) {
        this.view = view;
      }

      const sort = params.get('sort');
      if (sort) {
        if (sort.endsWith('Dsc')) {
          this.sortDsc = true;
          this.sortBy = sort.slice(0, -3);
        } else if (sort.endsWith('Asc')) {
          this.sortDsc = false;
          this.sortBy = sort.slice(0, -3);
        }
      }

      const page = params.get('page');
      if (page) {
        const parsedPage = Number.parseInt(page, 10);
        if (!Number.isNaN(parsedPage)) {
          this.page = Math.max(0, parsedPage);
        }
      }

      const actions = params.getAll('action');
      if (actions.length > 0) {
        this.actionFilters = new Set(actions);
      }

      const rules = params.getAll('rule');
      if (rules.length > 0) {
        this.ruleFilters = new Set(rules);
      }
    } catch {
      // Ignore URL parsing errors in non-browser or mock environments.
    }
  }

  /**
   * Synchronizes current query state to window.location via history.replaceState.
   */
  updateURL() {
    try {
      if (typeof window === 'undefined' || !window.location || !window.history) {
        return;
      }
      const url = new URL(window.location.href);
      if (this.searchQuery) {
        url.searchParams.set('q', this.searchQuery);
      } else {
        url.searchParams.delete('q');
      }
      if (this.view) {
        url.searchParams.set('view', this.view);
      } else {
        url.searchParams.delete('view');
      }
      const sortParam = this.sortBy + (this.sortDsc ? 'Dsc' : 'Asc');
      if (sortParam !== 'readyAsc') {
        url.searchParams.set('sort', sortParam);
      } else {
        url.searchParams.delete('sort');
      }
      if (this.page > 0) {
        url.searchParams.set('page', String(this.page));
      } else {
        url.searchParams.delete('page');
      }
      url.searchParams.delete('action');
      for (const action of this.actionFilters) {
        url.searchParams.append('action', action);
      }
      url.searchParams.delete('rule');
      for (const rule of this.ruleFilters) {
        url.searchParams.append('rule', rule);
      }
      window.history.replaceState({}, '', url.toString());
    } catch {
      // Ignore history navigation errors in non-browser or restricted iframe environments.
    }
  }

  /**
   * Updates the URL and dispatches a 'change' event to all listeners.
   */
  notifyChange() {
    this.updateURL();
    this.dispatchEvent(new CustomEvent('change'));
  }

  /**
   * Updates the search query string and resets page to 0.
   * @param {string} q
   */
  setSearchQuery(q) {
    if (this.searchQuery === q) {
      return;
    }
    this.searchQuery = q;
    this.page = 0;
    this.notifyChange();
  }

  /**
   * Updates the active view filter and resets page to 0.
   * @param {string} view
   */
  setView(view) {
    if (this.view === view) {
      return;
    }
    this.view = view;
    this.page = 0;
    this.notifyChange();
  }

  /**
   * Toggles the presence of an action in the action filter set.
   * @param {string} action
   */
  toggleActionFilter(action) {
    if (this.actionFilters.has(action)) {
      this.actionFilters.delete(action);
    } else {
      this.actionFilters.add(action);
    }
    this.page = 0;
    this.notifyChange();
  }

  /**
   * Toggles the presence of a rule in the rule filter set.
   * @param {string} rule
   */
  toggleRuleFilter(rule) {
    if (this.ruleFilters.has(rule)) {
      this.ruleFilters.delete(rule);
    } else {
      this.ruleFilters.add(rule);
    }
    this.page = 0;
    this.notifyChange();
  }

  /**
   * Toggles the sort column or reverses sort order if already sorted by column.
   * @param {string} col
   */
  toggleSort(col) {
    if (this.view === 'criticalPath') {
      return;
    }
    if (this.sortBy === col) {
      this.sortDsc = !this.sortDsc;
    } else {
      this.sortBy = col;
      this.sortDsc = false;
    }
    this.page = 0;
    this.notifyChange();
  }

  /**
   * Calculates the 0-indexed last page number.
   * @param {number} totalItems
   * @return {number}
   */
  calcPageLast(totalItems) {
    return Math.max(0, Math.ceil(totalItems / this.itemsPerPage) - 1);
  }

  /**
   * Sets the current page clamped between 0 and last page.
   * @param {number} targetPage
   * @param {number} totalItems
   */
  setPage(targetPage, totalItems) {
    const pageLast = this.calcPageLast(totalItems);
    const newPage = Math.max(0, Math.min(targetPage, pageLast));
    if (this.page !== newPage) {
      this.page = newPage;
      this.notifyChange();
    }
  }

  /**
   * Computes pagination boundaries and display indices.
   * @param {number} totalItems
   * @return {{
   *   totalItems: number,
   *   pageIndex: number,
   *   pageLast: number,
   *   itemsFirst: number,
   *   itemsLast: number,
   *   itemFirstLogical: number,
   *   itemLastLogical: number
   * }}
   */
  getPagination(totalItems) {
    const pageLast = this.calcPageLast(totalItems);
    const pageIndex = Math.max(0, Math.min(this.page, pageLast));
    const itemsFirst = pageIndex * this.itemsPerPage;
    const itemsLast = Math.min(itemsFirst + this.itemsPerPage, totalItems);
    const itemFirstLogical = totalItems === 0 ? 0 : itemsFirst + 1;
    const itemLastLogical = itemsLast;
    return {
      totalItems,
      pageIndex,
      pageLast,
      itemsFirst,
      itemsLast,
      itemFirstLogical,
      itemLastLogical,
    };
  }
}

export const stepQueryState = new StepQueryState();
