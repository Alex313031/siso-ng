// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

/**
 * Step JSON for the most recently requested build.
 *
 * Keyed by build ID rather than URL: the build ID identifies the data
 * regardless of where it was loaded from, so other sources (e.g. a locally
 * opened metrics file) can share the same cache in future.
 *
 * Only one build is kept, which covers switching between pages (e.g. steps
 * and aggregates) of the same build. To keep more builds (e.g. to compare
 * builds side-by-side), remove the `cache.clear()` call in `fetchSteps`.
 *
 * @type {!Map<string, !Promise<object>>}
 */
const cache = new Map();

/**
 * Returns step metrics data for a build, fetching it from the endpoint if it
 * is not already cached. The endpoint is only used on a cache miss.
 * Failed fetches are removed from the cache so subsequent requests can retry.
 *
 * Aborting `signal` rejects this call but never cancels the shared request,
 * since other callers may be waiting on it.
 *
 * @param {string} buildId
 * @param {string} [endpoint]
 * @param {{signal?: AbortSignal}} [options]
 * @return {!Promise<object>}
 */
export async function fetchSteps(buildId, endpoint, { signal } = {}) {
  if (!buildId) {
    throw new Error('Build ID is required');
  }
  signal?.throwIfAborted();
  if (!cache.has(buildId)) {
    if (!endpoint) {
      throw new Error('Endpoint is required');
    }
    // Only keep one build at a time.
    cache.clear();
    const promise = fetch(endpoint)
      .then((response) => {
        if (!response.ok) {
          throw new Error(`HTTP error! status: ${response.status}`);
        }
        return response.json();
      })
      .catch((err) => {
        // Only evict if a newer request hasn't replaced this entry.
        if (cache.get(buildId) === promise) {
          cache.delete(buildId);
        }
        throw err;
      });
    cache.set(buildId, promise);
  }
  const data = await cache.get(buildId);
  signal?.throwIfAborted();
  return data;
}

/**
 * Clears the cached build.
 */
export function clearStepsCache() {
  cache.clear();
}
