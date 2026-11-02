# Mermaid, vendored

`mermaid-12.2.0.min.js` is `dist/mermaid.min.js` from the npm package `mermaid@12.2.0` (MIT, see `LICENSE`),
copied unchanged so the docs site makes no request to a third-party CDN. Zensical loads Mermaid from unpkg only
when no `mermaid` global exists; `extra_javascript` in `zensical.toml` defines it first.

SHA-256: `a8a6a6f805b074a8261a2825c86f99c8054acb3b35f50bb7e24a4f9f2c3c3b9f`
(npm integrity of the package: `sha512-w7nr/SFrLO7BaVTs3dQTvYSV6nYSBj0Yy9DjBhsIJtAcHqRzXSq4Pg32ujHZ/AdWsAMNEvvhbm5dt9E2tOiHzw==`).

Dependabot does not see this file. To update: `npm pack mermaid@<version>`, check the integrity against
`npm view mermaid@<version> dist.integrity`, copy `package/dist/mermaid.min.js` and `package/LICENSE` here, rename
the file, update `extra_javascript` and this README, and render every diagram in `docs/design.md` before merging.
