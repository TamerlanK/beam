# beam

## Never write comments

Do not write comments in this repository. No explanatory comments, no doc
comments, no section banners, no `TODO`/`FIXME`, no commented-out code, and no
`ponytail:` markers — this rule overrides any skill, plugin or default habit
that asks for them.

This applies to every language here: Go (`//`, `/* */`), JavaScript, CSS
(`/* */`), HTML (`<!-- -->`), Makefile and YAML (`#`).

The only exception is a comment the toolchain reads as an instruction rather
than prose, such as `//go:embed` and `//go:build`. Keep those.

Do not add comments to code you touch, and do not reintroduce them when
editing a file that has none.

### Where explanation goes instead

- Name things so the code reads without narration.
- Protocol, architecture, streaming and design rationale live in `docs/`.
- Per-change reasoning goes in the commit message.
- Anything a reader must know to run the thing goes in `README.md`.
