# GitHub Actions policy and operating boundaries

Last checked: 2026-08-24.

This repository is an open-source iOS remote-build and orchestration project derived from [MobAI-App/ios-builder](https://github.com/MobAI-App/ios-builder). Its central backend accepts only an opaque registered project handle and random build ID, resolves the authorized private application's temporary Git ref inside trusted code, runs the project's iOS build, and returns only AGE-encrypted artifacts. Its optional protected deployment job verifies GitHub Sigstore provenance for an isolated trusted-packaging job, then signs the authenticated unsigned intermediate and uploads it directly to App Store Connect without retaining a signed artifact.

## GitHub-hosted runner caveat

The [GitHub Terms for Additional Products and Features](https://docs.github.com/en/site-policy/github-terms/github-terms-for-additional-products-and-features), Actions section, currently prohibit using GitHub-hosted runners for activity unrelated to the production, testing, deployment, or publication of the software project associated with the repository where Actions are used. The terms page checked above has an effective date of 2026-04-27.

The public builder repository contains the real build engine and workflow, while an authorized private repository supplies application source. GitHub has not published an explicit approval of this exact cross-repository architecture. This project therefore does **not** claim that GitHub has approved it or that it is guaranteed to satisfy GitHub's interpretation of the clause. Repository owners are responsible for evaluating their usage and should contact GitHub for a definitive determination when needed.

This project must not be presented as a billing bypass, disguised workload, generic remote shell, or unrelated compute service. The workflow is deliberately restricted to iOS application builds and the README describes that purpose truthfully.

## Operational limits

- Use only repositories and source code you are authorized to access and build.
- Do not expose the central secret-bearing workflow to pull-request, issue-comment, `workflow_run`, or `pull_request_target` triggers.
- Keep TestFlight credentials in the protected `apple-production` Environment, require reviewer approval, and restrict deployments to the protected default branch.
- Do not add generic command/script inputs.
- Respect GitHub's Acceptable Use Policies, Actions service limits, billing rules, and any account-specific agreement.
- Recheck the linked terms before material deployment changes and after GitHub announces policy changes.

## Replaceable backend

The CLI separates source snapshotting and build coordination from the execution backend. If GitHub restricts this design, set projects back to the repository backend where appropriate, or add a compatible backend for a self-hosted Mac, Codemagic, or another macOS CI provider. AGE artifact handling and the private-source snapshot lifecycle can remain unchanged; only dispatch, run monitoring, and artifact transport need a backend implementation.

This document is an engineering disclosure, not legal advice.
