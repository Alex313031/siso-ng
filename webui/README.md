# Siso web UI

Siso's web UI is an experimental visualizer for Siso build metrics.

It also provides a replacement for Ninja's `ninja -t browse`.

## Development practices

Siso does not have dedicated web frontend engineers, and it is not intended
for the web UI to be a substitute for systems such as CI.

As such, the web UI is developed as a server-side rendered webapp, keeping
external dependencies as minimal as reasonably possible.

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

Prefer writing as little custom JavaScript as possible.

For example:

- Popovers can be treated as native to the web platform as part of
  [Baseline 2025][popover-baseline] and can be utilized without JavaScript.
- `<md-dialog>` from Material Web Components provides dialog behaviors, and
  can be utilized without custom JavaScript.

Prefer avoiding technologies that require a web bundler.

Requiring an additional build step is strongly undesirable. The current web UI
takes advantage of modern web platform features that obviate many historical
reasons to use a web bundler.

For example:

- CSS nesting (including auxiliary features such as the `&` selector) is
  available as part of [Baseline 2023][css-nesting-baseline].

[mwc]: https://github.com/material-components/material-web
[htmx]: https://htmx.org/
[popover-baseline]: https://web.dev/blog/popover-baseline
[css-nesting-baseline]: https://web.dev/blog/baseline2023#more-features

