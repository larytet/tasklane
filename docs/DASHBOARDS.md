# Dashboards

A dashboard is a page of gadgets. Each gadget is a question about the issues (a saved filter, or a query written in the gadget) and a way of showing the answer. Open `#/dashboards`.

## Who sees what

A gadget is computed **for whoever is looking**, never for its author: the same dashboard shows each viewer only the issues their permissions let them see, in their own organization. A dashboard shared with the organization is visible to everyone in it; sharing a dashboard shares the layout, not the data. A saved filter shared with the organization works the same way.

Only the owner (and an organization administrator) can change a dashboard. Anyone who can see it can copy it, mark it a favourite, or make it their default.

## Gadgets

| Kind | Shows |
|---|---|
| Filter results | the issues a filter or query finds |
| Assigned to me | open issues assigned to the viewer |
| Activity stream | recent changes to the issues a query finds |
| Pie | a count of issues by a field (status, priority, assignee, project, type, label, ...) |
| Two-dimensional statistics | counts by two fields |
| Created vs resolved | issues created and resolved per day, for 7 to 90 days |
| Average age | the average age of open issues, and the oldest |
| Workload | open issues and estimates per assignee on a board |
| Sprint burndown | committed and completed in a sprint |
| Text | notes; always shown as text, never as markup |
| Embedded page | a page from an allowed host, in a sandboxed frame |

Every gadget has a refresh button, and the dashboard can refresh itself every 30 seconds to 15 minutes while the tab is visible. A gadget whose filter was deleted or is no longer visible says so in its own box; the others are unaffected.

## Embedded pages

An embedded page must be `https` and its host must be in `dashboard_iframe_hosts` (a comma-separated list; empty means no embedding). The page's Content-Security-Policy allows frames from exactly those hosts. The frame is sandboxed and sends no referrer.

## Not built yet

Heat map, sprint health, release burndown and "time since" gadgets; a gadget's saved-filter picker lists filters but filters cannot yet be edited in the UI (use the API).
