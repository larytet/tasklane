# Tasklane Helm chart

```
helm install tasklane deploy/helm/tasklane \
  --set config.base_url=https://tasklane.example.com \
  --set database.existingSecret=tasklane-db \
  --set admin.existingSecret=tasklane-admin
```

What it installs: the application (a Deployment), a Service, an optional Ingress, a PersistentVolumeClaim for attachments (or S3 settings),
an optional PodDisruptionBudget and NetworkPolicy, an optional `tasklane migrate` hook Job, and a `helm test` pod.

What it does not install: PostgreSQL 15 or newer (with `pg_trgm` and `unaccent`), an SMTP relay, an S3 store. They are references
(`database.*`, `smtp.*`, `blobs.s3.*`). There is no Redis setting because the application has none.

Secrets are never values: the database URL, the administrator, the secret key, the SMTP password and the S3 keys are read from Secrets you
create (`existingSecret` and `passwordSecret.name`). Anything the chart can say wrong is refused at install time: no database, a host
without a password Secret, S3 without a bucket, several replicas with attachments on a volume one node can mount, no address, and a grace
period too short for the drain.

Stopping a pod: it fails readiness, waits `lifecycle.drainDelay` for the load balancer, tells open pages to reconnect and finishes in-flight
requests; `terminationGracePeriodSeconds` must exceed that. Rolling updates keep every old pod until a new one is ready (`maxUnavailable: 0`).
Migrations run at start (one node at a time under a database lock); set `migrations.job.enabled` to run them before the new pods instead.

Tests: `go test ./internal/helmtest` renders the chart with a stand-in for Helm's template engine and checks the objects, the guards and
that every setting is one the application defines. CI with Helm installed should also run `helm lint` and `helm template`.
