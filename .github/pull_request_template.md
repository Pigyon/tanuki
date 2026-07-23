## What this changes

<!-- One or two sentences. What breaks if this is wrong? -->

## Why

<!-- Link the issue, or describe the engagement scenario that motivated it. -->

## Checklist

- [ ] `go test -race ./...` passes
- [ ] `golangci-lint run` is clean
- [ ] Tests cover the change (and assert the **real value is absent** from
      output, if this touches rewriting)
- [ ] README / CLI help updated if user-visible behaviour changed

## OpSec review

- [ ] This cannot cause real target data to reach the upstream API
- [ ] No new mapping type, detection rule, or hook path was added without a
      corresponding round-trip test

<!-- If either box is unchecked, explain here. -->
