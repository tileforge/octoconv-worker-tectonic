# octoconv-worker-tectonic

Tectonic worker for OctoConv. One process serves one approved manifest over Worker Protocol v2.

The process reads `TECTONIC_PROTOCOL_CORE_URL`, `TECTONIC_PROTOCOL_CLIENT_CERT`, `TECTONIC_PROTOCOL_CLIENT_KEY`, `TECTONIC_PROTOCOL_CLIENT_CA`, `TECTONIC_PROTOCOL_WORKER_ID`, and `TECTONIC_PROTOCOL_MANIFEST_PATH`. It refuses to start when `DATABASE_URL` or `REDIS_ADDR` is set. `TECTONIC_WORKER_CONCURRENCY` must be unset or `1`. Hello and pull advertise one slot, and one conversion runs at a time.

`schema/manifest-tectonic-convert.json` is capability `octoconv.convert.tectonic` version 1. The only pair is latex to pdf. `parameter_schema` is empty. Any parameter key fails before tectonic starts. Ordinal 0 is the document. Further inputs, up to 32, are written beside it under the filename in the presigned key `uploads/{job_id}/{ordinal}-{filename}`. A name the worker itself uses, or a name that is not a single plain file, fails the assignment.

`tectonic` must be on `PATH`. The default engine budget is 271s (`TECTONIC_ENGINE_TIMEOUT`). The default CPU budget is 271s (`TECTONIC_ENGINE_CPU_SECONDS`). The input ceiling is 2 MiB. The default output ceiling is 32 MiB (`TECTONIC_MAX_OUTPUT_BYTES`). The bundle path is `TECTONIC_BUNDLE_PATH`, default `/opt/tectonic/octoconv-bundle.ttb`. Landlock uses the tectonic ruleset from the core worker. `TECTONIC_SANDBOX_DISABLED=true` turns filesystem confinement off and still applies the CPU budget. A sandbox setup failure exits 78. The manifest is not approved.

`schema/manifest-tectonic-render.json` is capability `octoconv.render.tectonic` version 1 in a separate file and is not approved: latex goes to png with an empty parameter_schema, render typesets with tectonic and fits one printed page with pdftoppm, a protocol assignment has no size and draws 1280, and direct Convert accepts only size from 256 through 2048.
