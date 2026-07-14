{{- /*
Shared helpers. The templates under components/ mirror
internal/install/templates/*.yaml.tmpl — the two install paths deploy one
manifest set (ADR-0019 decision b), enforced by the golden-render drift guard
in internal/install/chart_golden_test.go: for equivalent values, `helm
template` must render byte-identical component documents to the Go path's
renderComponents. Edit both sides together, and keep components/ strictly the
renderComponents mirror set — chart-only extras (the bootstrap Job, node prep)
live outside it or the golden compare fails.
*/ -}}

{{- define "orkano.imageTag" -}}
{{- .Values.images.tag | default .Chart.AppVersion -}}
{{- end -}}
