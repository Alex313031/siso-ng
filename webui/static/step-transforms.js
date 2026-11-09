// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

/**
 * Traces the critical path through steps backwards using step.prev pointers.
 * Assumes the last step is on the critical path.
 * @param {!Array<!object>} steps
 * @return {!Array<!object>}
 */
export function computeCriticalPath(steps) {
  if (!steps || steps.length === 0) {
    return [];
  }
  const stepMap = new Map();
  for (const step of steps) {
    if (step.step_id) {
      stepMap.set(step.step_id, step);
    }
  }
  let curr = steps.at(-1);
  const path = [];
  const visited = new Set();
  while (curr && !visited.has(curr.step_id)) {
    visited.add(curr.step_id);
    path.push(curr);
    curr = curr.prev ? stepMap.get(curr.prev) : null;
  }
  path.reverse();
  return path;
}

/**
 * Filters a list of steps according to the query state.
 * @param {!Array<!object>} steps
 * @param {!import('./step-query-state.js').StepQueryState} queryState
 * @return {!Array<!object>}
 */
export function filterSteps(steps, queryState) {
  if (!steps || steps.length === 0) {
    return [];
  }

  if (queryState.view === 'criticalPath') {
    return computeCriticalPath(steps);
  }

  return steps.filter((step) => {
    if (queryState.view === 'localOnly' && !step.is_local) {
      return false;
    }
    if (queryState.searchQuery) {
      const output = step.outputs?.[0] || '';
      if (!output.includes(queryState.searchQuery)) {
        return false;
      }
    }
    if (
      queryState.actionFilters &&
      queryState.actionFilters.size > 0 &&
      !queryState.actionFilters.has(step.action)
    ) {
      return false;
    }
    if (
      queryState.ruleFilters &&
      queryState.ruleFilters.size > 0 &&
      !queryState.ruleFilters.has(step.rule)
    ) {
      return false;
    }
    return true;
  });
}

/**
 * Sorts steps by ready, duration, or completion time (asc/dsc).
 * In criticalPath view, execution order is preserved.
 * @param {!Array<!object>} steps
 * @param {!import('./step-query-state.js').StepQueryState} queryState
 * @return {!Array<!object>}
 */
export function sortSteps(steps, queryState) {
  const result = [...steps];
  if (queryState.view === 'criticalPath') {
    // Critical path preserves execution order.
    return result;
  }

  result.sort((a, b) => {
    const readyA = a.ready_nanos || 0;
    const readyB = b.ready_nanos || 0;
    const durA = a.duration_nanos || 0;
    const durB = b.duration_nanos || 0;

    let valA;
    let valB;
    if (queryState.sortBy === 'duration') {
      valA = durA;
      valB = durB;
    } else if (queryState.sortBy === 'completion') {
      valA = readyA + durA;
      valB = readyB + durB;
    } else {
      valA = readyA;
      valB = readyB;
    }

    const diff = valA - valB;
    return queryState.sortDsc ? -diff : diff;
  });
  return result;
}

/**
 * Formats an interval in nanoseconds to "XmYY.ZZs".
 *
 * TODO: add unit tests.
 *
 * @param {number} ns
 * @return {string}
 */
export function formatIntervalMetricTimestamp(ns) {
  if (ns === null || ns === undefined || Number.isNaN(ns)) {
    return '0m00.00s';
  }
  const totalMs = ns / 1e6;
  const minute = Math.floor(totalMs / 60000);
  const second = Math.floor((totalMs % 60000) / 1000);
  const ms = Math.round((totalMs % 1000) / 10);
  return `${minute}m${String(second).padStart(2, '0')}.${String(ms).padStart(2, '0')}s`;
}

/**
 * Formats an interval in nanoseconds into a human-friendly duration.
 *
 * TODO: add unit tests.
 *
 * @param {number} ns
 * @return {string}
 */
export function formatIntervalMetricHuman(ns) {
  if (ns === null || ns === undefined || Number.isNaN(ns)) {
    return '0.00s';
  }
  const ms = ns / 1e6;
  if (ms > 10) {
    const totalSec = ms / 1000;
    const mins = Math.floor(totalSec / 60);
    const secs = totalSec % 60;
    if (mins > 0) {
      return `${mins}m${secs < 10 ? '0' : ''}${secs.toFixed(2)}s`;
    }
    return `${secs.toFixed(2)}s`;
  } else {
    const us = (ns % 1e6) / 1000;
    return `${Math.floor(ms)}.${String(Math.round(us / 10)).padStart(2, '0')}ms`;
  }
}

/**
 * Trims the outdirRel prefix from an output path if present.
 * @param {string} output
 * @param {string} outdirRel
 * @return {string}
 */
export function trimOutputPrefix(output, outdirRel) {
  if (!output) {
    return '';
  }
  if (outdirRel && output.startsWith(outdirRel + '/')) {
    return output.slice(outdirRel.length + 1);
  }
  return output;
}

/**
 * Computes aggregated metrics grouped by rule (or action if rule is absent).
 * @param {!Array<!object>} steps
 * @return {!Array<!object>}
 */
export function computeAggregates(steps) {
  if (!steps || steps.length === 0) {
    return [];
  }
  const aggregates = new Map();
  for (const step of steps) {
    const key = step.rule || step.action || '';
    let entry = aggregates.get(key);
    if (!entry) {
      entry = {
        aggregateBy: key,
        isRule: Boolean(step.rule),
        count: 0,
        totalUtime: 0,
        totalDuration: 0,
        totalWeightedDuration: 0,
      };
      aggregates.set(key, entry);
    }
    entry.count++;
    entry.totalUtime += step.utime_nanos || 0;
    entry.totalDuration += step.duration_nanos || 0;
    entry.totalWeightedDuration += step.weighted_duration_nanos || 0;
  }
  return Array.from(aggregates.values());
}

