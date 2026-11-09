// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

import { describe, it, beforeEach, afterEach } from 'node:test';
import assert from 'node:assert/strict';
import { fetchSteps, clearStepsCache } from './steps-store.js';

/**
 * Returns a fetch mock that records requested URLs and responds with
 * `{ url }` as JSON.
 */
function recordingFetch() {
  const urls = [];
  const fetchFn = async (url) => {
    urls.push(url);
    return {
      ok: true,
      status: 200,
      json: async () => ({ url }),
    };
  };
  return { urls, fetchFn };
}

describe('steps-store', () => {
  let originalFetch;

  beforeEach(() => {
    originalFetch = globalThis.fetch;
    clearStepsCache();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    clearStepsCache();
  });

  it('fetches and returns step JSON data', async () => {
    const mockData = { steps: [{ step_id: 'step-1', rule: 'cxx' }] };
    globalThis.fetch = async (url) => {
      assert.equal(url, '/api/steps');
      return {
        ok: true,
        status: 200,
        json: async () => mockData,
      };
    };

    const data = await fetchSteps('build-1', '/api/steps');
    assert.deepEqual(data, mockData);
  });

  it('rejects when build ID is missing', async () => {
    globalThis.fetch = async () => {
      assert.fail('fetch should not be called');
    };
    await assert.rejects(fetchSteps('', '/api/steps'), {
      message: 'Build ID is required',
    });
  });

  it('rejects on cache miss when endpoint is missing', async () => {
    globalThis.fetch = async () => {
      assert.fail('fetch should not be called');
    };
    await assert.rejects(fetchSteps('build-1'), {
      message: 'Endpoint is required',
    });
  });

  it('serves cache hits without an endpoint', async () => {
    const { fetchFn } = recordingFetch();
    globalThis.fetch = fetchFn;

    await fetchSteps('build-1', '/api/steps');
    const data = await fetchSteps('build-1');
    assert.deepEqual(data, { url: '/api/steps' });
  });

  it('shares one fetch across multiple callers for the same build', async () => {
    const { urls, fetchFn } = recordingFetch();
    globalThis.fetch = fetchFn;

    // Simulate step table and aggregates table requesting the same build
    const [stepsData, aggregatesData] = await Promise.all([
      fetchSteps('123', '/builds/123/api/steps'),
      fetchSteps('123', '/builds/123/api/steps'),
    ]);

    assert.equal(urls.length, 1);
    assert.deepEqual(stepsData, aggregatesData);

    // Subsequent call also uses the cached promise
    await fetchSteps('123', '/builds/123/api/steps');
    assert.equal(urls.length, 1);
  });

  it('shares cached data for the same build across different endpoints', async () => {
    const { urls, fetchFn } = recordingFetch();
    globalThis.fetch = fetchFn;

    const first = await fetchSteps('abc', '/out/Default/builds/abc/api/steps');
    const second = await fetchSteps('abc', '/uploads/view/builds/abc/api/steps');

    assert.deepEqual(urls, ['/out/Default/builds/abc/api/steps']);
    assert.equal(second, first);
  });

  it('replaces the cached build when a different build is requested', async () => {
    const { urls, fetchFn } = recordingFetch();
    globalThis.fetch = fetchFn;

    await fetchSteps('1', '/builds/1/api/steps');
    await fetchSteps('2', '/builds/2/api/steps');
    await fetchSteps('2', '/builds/2/api/steps');
    await fetchSteps('1', '/builds/1/api/steps');

    assert.deepEqual(urls, [
      '/builds/1/api/steps',
      '/builds/2/api/steps',
      '/builds/1/api/steps',
    ]);
  });

  it('does not cache failed fetches (HTTP error)', async () => {
    let attempts = 0;
    globalThis.fetch = async () => {
      attempts++;
      if (attempts === 1) {
        return { ok: false, status: 500 };
      }
      return {
        ok: true,
        status: 200,
        json: async () => ({ success: true }),
      };
    };

    await assert.rejects(fetchSteps('err', '/builds/err/api/steps'), {
      message: 'HTTP error! status: 500',
    });
    assert.equal(attempts, 1);

    const data = await fetchSteps('err', '/builds/err/api/steps');
    assert.equal(attempts, 2);
    assert.deepEqual(data, { success: true });
  });

  it('does not cache network errors', async () => {
    let attempts = 0;
    globalThis.fetch = async () => {
      attempts++;
      if (attempts === 1) {
        throw new Error('Network failure');
      }
      return {
        ok: true,
        status: 200,
        json: async () => ({ success: true }),
      };
    };

    await assert.rejects(fetchSteps('neterr', '/builds/neterr/api/steps'), {
      message: 'Network failure',
    });
    assert.equal(attempts, 1);

    const data = await fetchSteps('neterr', '/builds/neterr/api/steps');
    assert.equal(attempts, 2);
    assert.deepEqual(data, { success: true });
  });

  it('does not clear a newer cached request when a replaced request rejects', async () => {
    let fetchCount = 0;
    let rejectFirst;
    const firstFetch = new Promise((_, reject) => {
      rejectFirst = reject;
    });
    globalThis.fetch = (url) => {
      fetchCount++;
      if (fetchCount === 1) {
        return firstFetch;
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ url, fetchCount }),
      });
    };

    // Request A, then replace it with B and then a fresh request for A.
    const request1 = fetchSteps('A', '/builds/A/api/steps');
    await fetchSteps('B', '/builds/B/api/steps');
    const request2 = fetchSteps('A', '/builds/A/api/steps');
    assert.equal(fetchCount, 3);

    // The first, replaced request for A now fails.
    rejectFirst(new Error('first failed'));
    await assert.rejects(request1, { message: 'first failed' });

    // The newer request for A must still be cached.
    const request3 = fetchSteps('A', '/builds/A/api/steps');
    assert.equal(fetchCount, 3);
    assert.deepEqual(await request3, await request2);
  });

  it('aborting rejects only that caller and does not cancel the shared request', async () => {
    let resolveFetch;
    let fetchSignal;
    let fetchCount = 0;
    globalThis.fetch = (url, init) => {
      fetchCount++;
      fetchSignal = init?.signal;
      return new Promise((resolve) => {
        resolveFetch = resolve;
      });
    };

    const controller = new AbortController();
    const aborted = fetchSteps('A', '/builds/A/api/steps', {
      signal: controller.signal,
    });
    const other = fetchSteps('A', '/builds/A/api/steps');
    controller.abort(new Error('aborted'));

    // A caller arriving after the abort still reuses the in-flight request.
    const next = fetchSteps('A', '/builds/A/api/steps');

    resolveFetch({ ok: true, status: 200, json: async () => ({ ok: 1 }) });
    await assert.rejects(aborted, { message: 'aborted' });
    assert.deepEqual(await other, { ok: 1 });
    assert.deepEqual(await next, { ok: 1 });
    assert.equal(fetchCount, 1);
    assert.equal(fetchSignal, undefined);
  });

  it('rejects immediately without fetching when the signal is already aborted', async () => {
    globalThis.fetch = async () => {
      assert.fail('fetch should not be called');
    };
    const controller = new AbortController();
    controller.abort(new Error('already aborted'));

    await assert.rejects(
      fetchSteps('A', '/builds/A/api/steps', { signal: controller.signal }),
      { message: 'already aborted' }
    );
  });

  it('clearStepsCache clears the cached build', async () => {
    const { urls, fetchFn } = recordingFetch();
    globalThis.fetch = fetchFn;

    await fetchSteps('build-1', '/api/steps');
    clearStepsCache();
    await fetchSteps('build-1', '/api/steps');

    assert.equal(urls.length, 2);
  });
});
