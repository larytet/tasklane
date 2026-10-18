# Versions and releases

A **version** is a release of one project. Open `#/releases` (or **Releases** in the top bar) to see a project's versions with progress, and to add, release, archive and delete them. Project administrators manage versions; anyone who can see the project sees them.

## On issues

An issue has **fix versions** (where the work ships) and **affects versions** (where a defect was found). Anyone who can edit the issue sets them from the issue screen, or with `PUT /api/v1/issues/{key}/versions` (`{"fix": ["2.1"], "affects": ["2.0"]}`; each list replaces the old one). Names are the project's own versions, matched without regard to case. An archived version can stay on an issue but cannot be added to another. Every change is in the issue's history (`fix_versions`, `affects_versions`).

## Progress

A version's progress counts the issues fixed in it by status category (to do, in progress, done), **for the person looking**: someone who cannot see an issue does not see it counted.

## Releasing

Releasing a version with issues that are not done does not release it: the first answer is a warning naming those issues. The administrator then chooses:

- **Move and release**: the unfinished issues move to another unreleased version of the project (with a history record each), and the version is released;
- **Release anyway** (`confirm`): released with the unfinished issues left on it; the answer lists them;
- cancel.

The release date becomes today unless one is given. A release can be undone (**Unrelease**). Deleting a version can move its issues to another version.

## Release notes

`GET /api/v1/versions/{id}/notes` (and **Notes** on the releases page) gives Markdown: the finished issues by issue type, the merged pull requests linked to them that the caller may see, then a **Not done** list. Text is shown as text.

## Searching

```
fixVersion = "2.1"
fixVersion in (releasedVersions())
fixVersion in (unreleasedVersions()) AND project = ABC
affectedVersion = "2.0"
fixVersion is EMPTY
```

`releasedVersions()` and `unreleasedVersions()` mean the versions with that state in the projects the caller can read. These fields are answered by PostgreSQL (a query using them does not use the search index).

## Not built yet

Calendar and roadmap views of releases, release burndown, a version field in the issue create form, versions in the JSON/CSV exchange bundles and the Jira importer, MCP tools for versions, mirrored (Jira) projects show no versions, automation triggers on "version released", and version reordering by drag and drop.
