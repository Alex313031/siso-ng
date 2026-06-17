# Siso web UI

Siso's web UI is an experimental visualizer for Siso build metrics.

It also provides a replacement for Ninja's `ninja -t browse`.

## Development philosophy

Siso does not have dedicated web frontend engineers, and it is not intended
for the web UI to be a substitute for systems such as CI.

As such, the web UI is developed as a server-side rendered webapp, keeping
external dependencies as minimal as reasonably possible.

### Dependencies

The only two major dependencies at time of writing are:

- **[Material Web Components][mwc]** provides off-the-shelf implementations
  of Material 3 components.
  - Custom CSS is used to override the default color palette.
  - Custom CSS is used to fill in and tweak missing behaviors.
- **[HTMX][htmx]** provides mechanisms to progressively enhance the server-side
  rendered webapp with client-side interactivity.
  - Most behaviors are attached using HTML attributes.
  - Custom JS is used to gracefully intercept and handle errors rather than
    forcing full-page refreshes.

### No custom JavaScript (within reason)

Prefer writing as little custom JavaScript as possible.

Frontend web frameworks add a burden of additional domain-specific expertise.
Without dedicated web frontend engineers, this adds maintenance overhead that
this project is not staffed to handle.

The modern web platform provides features that support rich client-side
interactivity with less overhead than historically required:

- Popovers can be treated as native to the web platform as part of
  [Baseline 2025][popover-baseline] and can be utilized without JavaScript.
- Web Components, such as [Material Web Components][mwc], can be utilized
  without a bundler and provide many common UI behaviors (e.g. dialogs, tabs,
  etc.) with custom design, interactivity, and accessibility story out of the
  box.

Unavoidable cases of custom JavaScript include:

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

