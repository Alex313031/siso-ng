// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Non-exported helper function for usage by the HTMX events below.
const showErrorModal = (titleText, contentHTML) => {
  const dialog = document.createElement('md-dialog');
  dialog.setAttribute('type', 'alert');
  dialog.setAttribute('open', 'open');
  // Get a unique number for identifying the form for <md-dialog>.
  let nowNumeric = new Date().valueOf();
  dialog.innerHTML = `
    <form id="md-dialog-errorform${nowNumeric}" method="dialog" slot="headline">
      ${titleText}
    </form>
    <div slot="content">${contentHTML}</div>
    <div slot="actions">
      <md-text-button form="md-dialog-errorform${nowNumeric}" value="ok">OK</md-text-button>
    </div>`;
  document.body.appendChild(dialog);
};

// HTMX send failures.
document.addEventListener('htmx:sendError', e => {
  showErrorModal(
    `Couldn't send request`,
    `<p>Is <code>siso webui</code> still running?</p>
    <p>Please <a href="https://issues.chromium.org/issues/new?component=1456832" target="_blank">file an issue</a> if this seems unexpected.</p>`);
});

// HTMX response failures.
document.addEventListener('htmx:responseError', e => {
  let responseURL = e?.detail?.xhr?.responseURL;
  // Extract the pathname of the URL. We don't care about the domain.
  if (responseURL && 'parse' in URL) {
    const parsedURL = URL.parse(responseURL);
    if (parsedURL) {
      responseURL = parsedURL.pathname;
    }
  }
  let response = e?.detail?.xhr?.response;
  let errorTitle = 'Unknown error';
  let errorMessage = '';
  // XHR response may be a string, so attempt to parse it if so.
  if (typeof response === 'string') {
    errorMessage = response;
    const parser = new DOMParser();
    const parsedResponse = parser.parseFromString(response, 'text/html')
    const errorNode = parsedResponse.querySelector('parsererror');
    if (!errorNode) {
      response = parsedResponse;
    }
  }
  // If XHR response can be a Document, try extract the known title/message portions.
  // If this fails, fall back on the defaults defined above.
  if (response instanceof Document) {
    const title = response.getElementById('page-error-title');
    if (title) {
      errorTitle = title.textContent;
    }
    const message = response.getElementById('page-error-message');
    if (message) {
      errorMessage = message.textContent;
    }
  }
  // Use a helper element to HTML escape the log.
  let helper = document.createElement('div');
  helper.textContent = `URL: ${responseURL}\nTitle: ${errorTitle}\nMessage: ${errorMessage}`;
  // Get a unique number for identifying the log to copy to clipboard.
  let nowNumeric = new Date().valueOf();
  showErrorModal(
    'Failed to process request',
    `Error log:
      <clipboard-copy for="copy-error${nowNumeric}">
        <md-icon-button>
          <md-icon>content_copy</md-icon>
        </md-icon-button>
      </clipboard-copy>
      <pre><code id="copy-error${nowNumeric}" class="error-log">${helper.innerHTML}</code></pre>
      <p>Please <a href="https://issues.chromium.org/issues/new?component=1456832" target="_blank">file an issue</a> if this seems unexpected.</p>`);
});
