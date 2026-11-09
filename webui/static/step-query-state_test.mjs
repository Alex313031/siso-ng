// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { StepQueryState } from './step-query-state.js';

describe('StepQueryState', () => {
  it('initializes with default values', () => {
    const state = new StepQueryState();
    assert.equal(state.searchQuery, '');
    assert.equal(state.view, '');
    assert.deepEqual(Array.from(state.actionFilters), []);
    assert.deepEqual(Array.from(state.ruleFilters), []);
    assert.equal(state.sortBy, 'ready');
    assert.equal(state.sortDsc, false);
    assert.equal(state.page, 0);
    assert.equal(state.itemsPerPage, 100);
  });

  it('resets to defaults when initFromURL is called with empty string after parameters', () => {
    const state = new StepQueryState();
    state.initFromURL('?q=a');
    assert.equal(state.searchQuery, 'a');

    state.initFromURL('');
    assert.equal(state.searchQuery, '');
    assert.equal(state.view, '');
    assert.deepEqual(Array.from(state.actionFilters), []);
    assert.deepEqual(Array.from(state.ruleFilters), []);
    assert.equal(state.sortBy, 'ready');
    assert.equal(state.sortDsc, false);
    assert.equal(state.page, 0);
  });

  it('parses all query parameters from URL', () => {
    const state = new StepQueryState();
    state.initFromURL('?q=hello&view=localOnly&sort=durationDsc&page=4&action=compile&action=link&rule=cxx');
    assert.equal(state.searchQuery, 'hello');
    assert.equal(state.view, 'localOnly');
    assert.equal(state.sortBy, 'duration');
    assert.equal(state.sortDsc, true);
    assert.equal(state.page, 4);
    assert.deepEqual(Array.from(state.actionFilters), ['compile', 'link']);
    assert.deepEqual(Array.from(state.ruleFilters), ['cxx']);
  });

  it('resets any omitted fields when re-initialized from partial URL', () => {
    const state = new StepQueryState();
    state.initFromURL('?q=hello&view=localOnly&sort=durationDsc&page=4&action=compile&rule=cxx');

    // Re-initialize with only sort=completionAsc
    state.initFromURL('?sort=completionAsc');
    assert.equal(state.searchQuery, '');
    assert.equal(state.view, '');
    assert.equal(state.sortBy, 'completion');
    assert.equal(state.sortDsc, false);
    assert.equal(state.page, 0);
    assert.deepEqual(Array.from(state.actionFilters), []);
    assert.deepEqual(Array.from(state.ruleFilters), []);
  });

  it('reset() resets all fields to default values', () => {
    const state = new StepQueryState();
    state.searchQuery = 'custom';
    state.view = 'customView';
    state.actionFilters.add('customAction');
    state.ruleFilters.add('customRule');
    state.sortBy = 'duration';
    state.sortDsc = true;
    state.page = 10;
    state.itemsPerPage = 50;

    state.reset();
    assert.equal(state.searchQuery, '');
    assert.equal(state.view, '');
    assert.deepEqual(Array.from(state.actionFilters), []);
    assert.deepEqual(Array.from(state.ruleFilters), []);
    assert.equal(state.sortBy, 'ready');
    assert.equal(state.sortDsc, false);
    assert.equal(state.page, 0);
    assert.equal(state.itemsPerPage, 100);
  });

  it('dispatches change events when setters are called', () => {
    const state = new StepQueryState();
    let changeEvents = 0;
    state.addEventListener('change', () => {
      changeEvents++;
    });

    state.setSearchQuery('new query');
    assert.equal(state.searchQuery, 'new query');
    assert.equal(changeEvents, 1);

    // Setting same query does not emit change
    state.setSearchQuery('new query');
    assert.equal(changeEvents, 1);

    state.setView('criticalPath');
    assert.equal(state.view, 'criticalPath');
    assert.equal(changeEvents, 2);

    state.toggleActionFilter('compile');
    assert.ok(state.actionFilters.has('compile'));
    assert.equal(changeEvents, 3);

    state.toggleActionFilter('compile');
    assert.ok(!state.actionFilters.has('compile'));
    assert.equal(changeEvents, 4);

    state.toggleRuleFilter('cxx');
    assert.ok(state.ruleFilters.has('cxx'));
    assert.equal(changeEvents, 5);

    // toggleSort should be ignored if view === 'criticalPath'
    state.toggleSort('duration');
    assert.equal(state.sortBy, 'ready');
    assert.equal(changeEvents, 5);

    // Exit criticalPath view and toggle sort
    state.setView('');
    assert.equal(changeEvents, 6);
    state.toggleSort('duration');
    assert.equal(state.sortBy, 'duration');
    assert.equal(state.sortDsc, false);
    assert.equal(changeEvents, 7);

    // Toggle same sort column reverses direction
    state.toggleSort('duration');
    assert.equal(state.sortBy, 'duration');
    assert.equal(state.sortDsc, true);
    assert.equal(changeEvents, 8);
  });

  it('calculates pagination correctly', () => {
    const state = new StepQueryState();
    state.itemsPerPage = 10;

    assert.equal(state.calcPageLast(0), 0);
    assert.equal(state.calcPageLast(5), 0);
    assert.equal(state.calcPageLast(10), 0);
    assert.equal(state.calcPageLast(11), 1);
    assert.equal(state.calcPageLast(25), 2);

    state.setPage(1, 25);
    assert.equal(state.page, 1);

    const pag = state.getPagination(25);
    assert.equal(pag.totalItems, 25);
    assert.equal(pag.pageIndex, 1);
    assert.equal(pag.pageLast, 2);
    assert.equal(pag.itemsFirst, 10);
    assert.equal(pag.itemsLast, 20);
    assert.equal(pag.itemFirstLogical, 11);
    assert.equal(pag.itemLastLogical, 20);
  });
});
