#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Generates dashboards/shape-overview.json (doc/outputs.md, Grafana).

Run from the repository root: python3 dashboards/gen/shape_overview.py
"""
import json

DS = {"type": "prometheus", "uid": "${datasource}"}
SEL = 'route=~"$route",model=~"$model"'
R = "$__rate_interval"

panels, y = [], 0


def row(title):
    global y
    panels.append({"type": "row", "title": title, "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "id": len(panels) + 1})
    y += 1


def panel(title, kind, targets, x, w, h=8, unit=None, desc=None, extra=None):
    p = {"type": kind, "title": title, "datasource": DS, "id": len(panels) + 1,
         "gridPos": {"h": h, "w": w, "x": x, "y": y},
         "targets": [dict(t, datasource=DS, refId=chr(65 + i)) for i, t in enumerate(targets)],
         "fieldConfig": {"defaults": {}, "overrides": []}, "options": {}}
    if unit:
        p["fieldConfig"]["defaults"]["unit"] = unit
    if desc:
        p["description"] = desc
    if extra:
        for k, v in extra.items():
            p[k] = v
    panels.append(p)


def q(expr, legend=None, **kw):
    t = {"expr": expr, "legendFormat": legend or "__auto"}
    t.update(kw)
    return t


def quantiles(metric, labels=SEL):
    return [q(f'histogram_quantile({p}, sum by (le) (rate({metric}_bucket{{{labels}}}[{R}])))', f"p{int(p*100)}")
            for p in (0.5, 0.9, 0.99)]


def heat(metric):
    return [q(f'sum by (le) (increase({metric}_bucket{{{SEL}}}[{R}]))', "{{le}}", format="heatmap")]


row("Traffic")
panel("Requests per second", "timeseries", [q(f'sum by (route) (rate(llm_shape_requests_total{{{SEL}}}[{R}]))', "{{route}}")], 0, 8, unit="reqps")
panel("In flight (all instances)", "timeseries", [q('sum by (route) (llm_shape_inflight{route=~"$route"})', "{{route}}")], 8, 8)
panel("Concurrency at arrival (per instance)", "timeseries", quantiles("llm_shape_concurrency_at_arrival", 'route=~"$route"'), 16, 8,
      desc="Requests already in flight on the route when a request arrived, on the instance that served it. Exact cross-instance concurrency comes from JSONL records.")
y += 8

row("Size")
panel("Prompt tokens", "heatmap", heat("llm_shape_prompt_tokens"), 0, 8, extra={"options": {"calculate": False, "yAxis": {"unit": "short"}}})
panel("Completion tokens", "heatmap", heat("llm_shape_completion_tokens"), 8, 8, extra={"options": {"calculate": False, "yAxis": {"unit": "short"}}})
panel("Input / output token ratio", "timeseries",
      [q(f'sum(rate(llm_shape_prompt_tokens_total{{{SEL}}}[{R}])) / sum(rate(llm_shape_completion_tokens_total{{{SEL}}}[{R}]))', "input/output")], 16, 8)
y += 8
# Cumulative counts, not increase() over the range: Prometheus cannot see the
# first increment of a new series, and shape cells are sparse, so increase()
# would undercount exactly the rare cells.
panel("Shape cells since proxy start: share of requests by prompt × output tokens (bucket upper bounds)", "table",
      [q(f'sum by (prompt_bucket, output_bucket) (llm_shape_cell_total{{{SEL}}}) / scalar(sum(llm_shape_cell_total{{{SEL}}}))',
         format="table", instant=True)], 0, 24, h=9, unit="percentunit",
      desc="Rows: prompt token bucket. Columns: output token bucket. Reported and estimated usage together. Counted since each proxy instance started; increase() over a time range would miss the first request of every new cell.",
      extra={"transformations": [{"id": "groupingToMatrix", "options": {"rowField": "prompt_bucket", "columnField": "output_bucket", "valueField": "Value"}}]})
y += 9

row("Latency")
panel("Time to first token", "timeseries", quantiles("llm_shape_ttft_seconds"), 0, 8, unit="s")
panel("Latency", "timeseries", quantiles("llm_shape_latency_seconds"), 8, 8, unit="s")
panel("Decode rate, median", "timeseries",
      [q(f'histogram_quantile(0.5, sum by (le) (rate(llm_shape_decode_tokens_per_second_bucket{{{SEL}}}[{R}])))', "p50 tokens/s")], 16, 8)
y += 8

row("Reuse")
panel("Upstream prefix cache hit share", "timeseries",
      [q(f'sum(rate(llm_shape_cached_prompt_tokens_total{{{SEL}}}[{R}])) / sum(rate(llm_shape_prompt_tokens_total{{{SEL},usage="reported"}}[{R}]))', "cached / input")],
      0, 12, unit="percentunit", desc="Only where the upstream reports cached tokens.")
panel("Usage source share", "timeseries",
      [q(f'sum by (usage) (rate(llm_shape_completion_tokens_count{{{SEL}}}[{R}])) / ignoring(usage) group_left sum(rate(llm_shape_completion_tokens_count{{{SEL}}}[{R}]))', "{{usage}}")],
      12, 12, unit="percentunit", desc="Reported by the upstream vs estimated by the proxy.")
y += 8

row("Health")
panel("Requests by status class", "timeseries", [q(f'sum by (status_class, outcome) (rate(llm_shape_requests_total{{{SEL}}}[{R}]))', "{{status_class}} {{outcome}}")], 0, 8, unit="reqps")
panel("Without usage (neither reported nor estimated)", "timeseries",
      [q(f'sum(rate(llm_shape_usage_missing_total{{{SEL}}}[{R}])) / sum(rate(llm_shape_requests_total{{{SEL}}}[{R}]))', "share")], 8, 8, unit="percentunit")
panel("Proxy internals", "timeseries", [
    q(f'sum(rate(llm_shape_events_dropped_total[{R}]))', "events dropped/s"),
    q('sum(llm_shape_event_queue_length)', "queue length"),
    q(f'sum(rate(llm_shape_stage2_panics_total[{R}]))', "stage-2 panics/s"),
    q(f'sum by (state) (rate(llm_shape_capture_total{{route=~"$route"}}[{R}]))', "capture {{state}}/s"),
], 16, 8)
y += 8
panel("Capture memory in use", "timeseries", [q('sum(llm_shape_capture_budget_used_bytes)', "bytes")], 0, 12, unit="bytes")
panel("Authentication failures", "timeseries", [q(f'sum by (reason) (rate(llm_shape_auth_failures_total[{R}]))', "{{reason}}")], 12, 12)

dashboard = {
    "title": "LLM workload shape", "uid": "llm-shape-overview", "schemaVersion": 39, "version": 1,
    "tags": ["llm-shape-proxy"], "time": {"from": "now-30m", "to": "now"}, "refresh": "10s",
    "templating": {"list": [
        {"name": "datasource", "type": "datasource", "query": "prometheus", "current": {"text": "Prometheus", "value": "prometheus"}},
        {"name": "route", "type": "query", "datasource": DS, "query": "label_values(llm_shape_requests_total, route)",
         "includeAll": True, "multi": True, "allValue": ".*", "current": {"text": "All", "value": "$__all"}, "refresh": 2},
        {"name": "model", "type": "query", "datasource": DS, "query": 'label_values(llm_shape_requests_total{route=~"$route"}, model)',
         "includeAll": True, "multi": True, "allValue": ".*", "current": {"text": "All", "value": "$__all"}, "refresh": 2},
    ]},
    "panels": panels,
}
with open("dashboards/shape-overview.json", "w") as f:
    json.dump(dashboard, f, indent=2)
    f.write("\n")
