// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

const toggleDarkMode = () => {
  const root = document.documentElement;
  const isDark = root.classList.contains('dark') ||
    (!root.classList.contains('light') && window.matchMedia('(prefers-color-scheme: dark)').matches);
  if (isDark) {
    root.classList.remove('dark');
    root.classList.add('light');
  } else {
    root.classList.remove('light');
    root.classList.add('dark');
  }
};

const PERFETTO_ORIGIN = 'https://ui.perfetto.dev';
const openInPerfetto = async (traceUrl) => {
  // TODO: check for and present errors.
  const resp = await fetch(traceUrl);
  const blob = await resp.blob();
  const arrayBuffer = await blob.arrayBuffer();
  // Technique to deep-link to Perfetto documented in:
  // https://github.com/google/perfetto/blob/7accba4e26bffa6b5a129cd73e3330d85301cf71/docs/visualization/deep-linking-to-perfetto-ui.md
  let handle = window.open('https://ui.perfetto.dev');
  const timer = setInterval(() => handle.postMessage('PING', PERFETTO_ORIGIN), 50);
  const onMessageHandler = (evt) => {
    if (evt.data !== 'PONG') return;
    window.clearInterval(timer);
    window.removeEventListener('message', onMessageHandler);
    const reopenUrl = new URL(location.href);
    reopenUrl.hash = `#reopen=${traceUrl}`;
    handle.postMessage({
      perfetto: {
        buffer: arrayBuffer,
        title: 'siso_trace.json',
        url: reopenUrl.toString(),
    }}, PERFETTO_ORIGIN);
  };
  window.addEventListener('message', onMessageHandler);
};

export { toggleDarkMode, openInPerfetto };
