# Releasing telemetry-web

Four modules live here: the core `telemetry-web` and one module per adapter. The
adapters `require` the core by version, so they can only be tagged **after** the
core version they name exists. That ordering is the whole of this document.

Separate adapter modules are deliberate: Go links only reachable packages, so a
chi user never *ships* gin code, but a single module would still put gin's
dependency graph into every consumer's `go.mod` and `go.sum`, and every scanner
that reads those files would page a chi user about a gin advisory.

---

## Why a pre-merge pin is not enough

A pseudo-version like `v0.0.0-20260803074602-1a8b64b4d44a` resolves only while the
commit is reachable. On a feature branch that means:

- `git branch -a --contains <sha>` lists the feature branch and nothing else
- a squash merge or a branch deletion makes the commit unreachable, and every
  adapter stops resolving
- `go get .../adapters/chi@latest` cannot work at all, because with no matching
  tag the proxy falls back to the default branch, where the module may not exist

So a pseudo-version pin is a development convenience, never a released state.

**Invariant:** an adapter must not be tagged while its core requirement is a
`v0.0.0-` pseudo-version. The release workflow enforces this; see
[the release gate](#the-release-gate).

---

## Release order

Tags are path-prefixed, which is what `go get` resolves.

1. **Merge the core to `master`.** Until then no tag can point at it.

2. **Tag the core.**

   ```sh
   git tag telemetry-web/v0.1.0 && git push origin telemetry-web/v0.1.0
   ```

3. **Repoint all three adapters at that tag, in one commit.**

   ```sh
   for m in gin echo chi; do
     (cd telemetry-web/adapters/$m &&
       go mod edit -require=github.com/stakater/operator-utils/telemetry-web@v0.1.0 &&
       GOWORK=off go mod tidy)
   done
   ```

   Then verify what a consumer will actually resolve, outside the workspace:

   ```sh
   for m in gin echo chi; do
     (cd telemetry-web/adapters/$m && GOWORK=off go build ./... && GOWORK=off go test -count=1 ./...)
   done
   ```

   Commit and merge that.

4. **Tag the adapters, on that later commit.**

   ```sh
   for m in gin echo chi; do
     git tag telemetry-web/adapters/$m/v0.1.0
   done
   git push origin --tags
   ```

Tagging all four on one commit is the mistake to avoid: the adapter tags would be
valid, but each would depend on a core pseudo-version instead of the core tag,
which is the state this process exists to prevent.

`go.work` stays in the repo. It is the right tool for developing the core and the
adapters together, and it is why step 3's verification sets `GOWORK=off`.

---

## The release gate

`.github/workflows/release.yml` triggers on `v*`, `telemetry-web/v*` and
`telemetry-web/adapters/*/v*`. In these filters `*` does not cross `/`, which is
why the three patterns are listed separately rather than as `telemetry-web/**`.

Before publishing an adapter tag the workflow asserts the invariant above: the
core requirement must be a real semver tag. That check belongs here rather than in
the PR job, because only here can it be satisfied.

---

## Docs that must change with the first tag

The install instructions currently tell consumers to pin a commit. Once
`telemetry-web/v0.1.0` exists they should name the tag instead:

- `README.md` (installation)
- `docs/reference.md` ("not yet tagged")
- `docs/guides/{gin,echo,chi}-adapter.md` and `docs/guides/echo-raw.md`
  (`go get ...@<commit-sha>`)
