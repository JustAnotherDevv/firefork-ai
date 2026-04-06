<!--
Thanks for the PR. Please fill out the sections below.
-->

## Summary

<!-- 1-3 sentences. What does this change do and why? -->

## Type of change

- [ ] Bug fix (non-breaking)
- [ ] Feature (non-breaking, additive)
- [ ] Breaking change (CLI flag, HTTP route, snapshot schema)
- [ ] Documentation only
- [ ] Build / CI / tooling

## Closes / references

<!-- e.g. Closes #42 -->

## Test plan

<!-- Checklist of what you verified locally. -->

- [ ] `make lint` clean
- [ ] `make test` passes
- [ ] `make test-int` passes (if change touches Firecracker boot path)
- [ ] Manual smoke test on a real Linux host (describe below)

<!-- Manual test details: -->

## Public-surface impact

<!-- Did you change CLI flags, HTTP routes, registry schema, or
exported package APIs? If yes, describe. If no, write "none". -->

## Threat-model impact (security-adjacent changes only)

<!-- For changes touching jailer config, vsock auth, snapshot decompression,
HMAC, file modes, or process privileges: 1-2 paragraphs on threat-model
implications. Leave blank otherwise. -->
