# Upstream Rebase Process

This repo vendors a forked `ovn-kubernetes` repo as a git submodule at
`ovn-kubernetes/`. The fork contains DPF-specific patches on top of upstream
`ovn-kubernetes`. Use this document to rebase the fork on the latest upstream
changes, update the submodule pointer in this repo, and push the results.

## Prerequisites

- You have cloned this repo locally.
- The submodule has an `origin` remote (your fork); you will add the `upstream` remote during setup.
- Your working trees (both the submodule and this repo) are clean.
- You will rebase onto `main`, and use a forked branch for test/validation if needed.

## Initialize and configure the submodule

1. From the top-level repo, initialize and update the submodule:
   - `git submodule update --init --recursive ovn-kubernetes`
2. Enter the submodule:
   - `cd ovn-kubernetes`
3. Verify remotes:
   - `git remote -v`
4. If `upstream` is missing, add it:
   - `git remote add upstream https://github.com/ovn-kubernetes/ovn-kubernetes`
5. Fetch latest refs:
   - `git fetch upstream --prune`
   - `git fetch origin --prune`

## Rebase the fork on upstream

1. In the submodule, checkout `main` and rebase it on upstream:
   - `cd ovn-kubernetes`
   - `git checkout main`
   - `git rebase upstream/master`
2. Resolve conflicts as needed, then continue the rebase:
   - `git status`
   - `git add <resolved-files>`
   - `git rebase --continue`
3. If you need to run tests or validation on your fork, create/use a forked branch for that instead of `main`:
   - `git checkout -b <fork-branch>`
4. Push the rebased `main` branch back to your fork:
   - `git push --force-with-lease origin main`

Notes:

- Use `--force-with-lease` because a rebase rewrites history.
- If the fork branch is shared, coordinate the rebase with other users.

## Update the submodule in this repo

1. Return to the top-level repo:
   - `cd ..`
2. Update the submodule pointer to the new fork commit:
   - `git status`
   - `git add ovn-kubernetes`
3. Commit and push the submodule update:
   - `git commit -m "Bump ovn-kubernetes submodule"`
   - `git push origin <branch>`

## Tags

The current taging strategy for the main branch is to use
`v<year>.<month>.<date>-<sha of HEAD>`.

for example `v26.1.27-ad5189a`

## Release

Follow the GitHub release process:

1. Go to Releases -> Draft a new release.
2. Pick a tag using the scheme in the Tags section and target `main`.
3. Use the tag as the release title.
4. Add a short summary of key changes and notable fixes, specifically a pointer to the ovn-kubernetes tag/sha.
5. Mark it as a pre-release/latest release as needed.
6. Publish the release.

## Artifacts

This results in the following artifcats.

Helm chart repo: `oci://ghcr.io/mellanox/charts/ovn-kubernetes-chart`

Helm chart version `v26.1.27-ad5189a`

Image: `ghcr.io/mellanox/ovn-kubernetes-dpf-fedora:v26.1.27-ad5189a`
