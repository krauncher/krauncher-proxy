# krauncher-proxy

Open logging proxy for LLM API traffic. It forwards requests and responses
unchanged and records the workload shape of the traffic, not its content.
Statistics are exported in a Grafana-compatible form.

Architecture: two queues — one for accepting and forwarding requests/responses,
one for processing them and writing statistics.
