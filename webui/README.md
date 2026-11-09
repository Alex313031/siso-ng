# Siso web UI

Siso's web UI is an experimental visualizer for Siso build metrics.

It also provides a replacement for Ninja's `ninja -t browse`.

## Development philosophy

The web UI was historically a minimal Go-rendered webapp to visualize Siso
build metrics, utilizing HTMX and modern web platform features to
progressively enhance statically-rendered pages with rich client-side
interactivity.

Due to increasing scope and complexity of the web UI, as well as the desire
to decouple the step metric viewer from Siso's internals, and the increasingly
clear benefits of being able to run without a backend, as of late 2026 the
web UI is undergoing a partial rewrite into a client-side webapp.

This therefore *drops* the progressive enhancement philosophy, and will require
JavaScript to be enabled for full functionality of Siso-only features.

Functionality that serves as a drop-in for Ninja features e.g. the Ninja rule
browser are currently **out of scope** of this decision.

### Dependencies

The only two major dependencies in the web UI's initial life were:

- **[Material Web Components][mwc]** provides off-the-shelf implementations
  of Material 3 components.
  - Custom CSS is used to override the default color palette.
  - Custom CSS is used to fill in and tweak missing behaviors.
- **[HTMX][htmx]** provides mechanisms to progressively enhance the server-side
  rendered webapp with client-side interactivity.
  - Most behaviors are attached using HTML attributes.
  - Custom JS is used to gracefully intercept and handle errors rather than
    forcing full-page refreshes.

Lit, a lightweight wrapper offering conveniences around the web platform's
native web components, came as a transitive dependency via Material Web
Components and thus is adopted for the late 2026 rewrite.

### Avoid inline scripts

To balance the realities of supporting custom client-side functionality
in a project not staffed with dedicated web frontend engineers, the initial
ground rule being set is that client-side only JavaScript being introduced
*must* be encapsulated via web components.

Dependencies should be kept as minimal as possible; **strongly** consider
native web platform features first.

Web components should be treated as "islands" of functionality that compose
together with clear contracts.

Pre-adoption of Lit, these were the justifications for custom JavaScript:

- Real-time events via Server-Sent Events are part of the modern web platform,
  but require custom JavaScript to handle.
  - Usage is abstracted by HTMX. See the below section for more information.
- Custom handlers for HTMX errors, so that they are gracefully handled instead
  of causing unexpected page breakages and/or requiring full page refreshes.
- Our [deep-linking to Perfetto UI][perfetto-deep-linking], due to Perfetto's
  security requirements.

### No custom build tools

Avoid technologies that require a web bundler.

Keeping the web UI buildable via the standard Go toolchain keeps development
and maintenance simpler.

The current web UI takes advantage of modern web platform features that obviate
historical reasons to use a web bundler, such as:

- CSS nesting (including auxiliary features such as the `&` selector),
  one historically common reason to require a build step for CSS preprocessing,
  is available as part of [Baseline 2023][css-nesting-baseline].

### JavaScript tests

Plain JavaScript modules (i.e. not web components) can be unit tested with
Node's built-in [`node:test`][node-test] runner and `node:assert`, without any
npm dependencies. Prefer moving logic out of web components and into such
modules so that it can be tested this way.

- Name test files `*_test.mjs` next to the module under test. The `.mjs`
  extension keeps them out of the `static/*.js` files embedded by `serve.go`.
- Run them from this directory with `node --test`.
- `NODEJS_VERSION` pins the Node version used by CI. The `buildinfra` recipe
  runs `node --test` in every directory that contains a `NODEJS_VERSION` file.

## Architecture

### Server-Sent Events

`sse.go` provides an SSE (Server-Sent Events) endpoint at `/events/` that can
be used to push updates to the browser, where polling would be inefficient or
cumbersome.

This endpoint is not coupled to any specific features in the Siso web UI. Any
handler may broadcast a message to all listeners by sending a message to the
Go channel:

```go
s.sseServer.messages <- sseMessage{"yourevent", "yourmessage"}
```

Templates may consume this using HTMX's SSE attributes, without requiring
additional custom JavaScript.

Given the above example, the below snippet would listen for the corresponding
event name, and display its contents in the `<div>` upon receipt:

```html
<div hx-ext="sse" sse-connect="/events/">
  <div sse-swap="yourevent" hx-target="this" hx-swap="innerHTML"></div>
</div>
```

All event filtering is performed client-side; all open instances of the Siso
web UI listening to the same endpoint will receive all messages. The `sse-swap`
attribute determines which of them are responded to.

[mwc]: https://github.com/material-components/material-web
[htmx]: https://htmx.org/
[perfetto-deep-linking]: https://perfetto.dev/docs/visualization/deep-linking-to-perfetto-ui
[popover-baseline]: https://web.dev/blog/popover-baseline
[css-nesting-baseline]: https://web.dev/blog/baseline2023#more-features
[node-test]: https://nodejs.org/api/test.html
