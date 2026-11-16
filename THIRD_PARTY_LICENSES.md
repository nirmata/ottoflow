# Third-Party License Attributions

This project (OttoFlow) uses the following third-party Go packages (as resolved by go-licenses; some rows are sub-packages of a shared module). Generated with `go-licenses` from `go.mod` on 2026-07-29.

## Summary

| License | Count |
|---|---|
| Apache-2.0 | 99 |
| MIT | 57 |
| BSD-3-Clause | 37 |
| BSD-2-Clause | 2 |
| MPL-2.0 | 1 |
| ISC | 1 |

**Total third-party dependencies:** 197

**Copyleft license check (GPL/AGPL/LGPL):** None found.

**Note:** `hashicorp/golang-lru/v2` is MPL-2.0 (weak/file-level copyleft — modifications to MPL-licensed *files* must be shared under MPL if redistributed; does not affect the rest of the codebase). Flagged for awareness, not a blocker.

---

## Apache-2.0

| Dependency | Source |
|---|---|
| `cel.dev/expr` | https://github.com/cel-expr/cel-spec/blob/v0.25.1/LICENSE |
| `cloud.google.com/go/auth` | https://github.com/googleapis/google-cloud-go/blob/auth/v0.18.2/auth/LICENSE |
| `cloud.google.com/go/civil` | https://github.com/googleapis/google-cloud-go/blob/v0.123.0/LICENSE |
| `cloud.google.com/go/compute/metadata` | https://github.com/googleapis/google-cloud-go/blob/compute/metadata/v0.9.0/compute/metadata/LICENSE |
| `github.com/GoogleCloudPlatform/kubectl-ai/gollm` | https://github.com/GoogleCloudPlatform/kubectl-ai/blob/08cf256aa2f5/gollm/LICENSE |
| `github.com/GoogleCloudPlatform/kubectl-ai/pkg` | https://github.com/GoogleCloudPlatform/kubectl-ai/blob/v0.0.31/LICENSE |
| `github.com/aws/aws-sdk-go-v2` | https://github.com/aws/aws-sdk-go-v2/blob/v1.41.5/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream` | https://github.com/aws/aws-sdk-go-v2/blob/aws/protocol/eventstream/v1.7.8/aws/protocol/eventstream/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/config` | https://github.com/aws/aws-sdk-go-v2/blob/config/v1.32.12/config/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/credentials` | https://github.com/aws/aws-sdk-go-v2/blob/credentials/v1.19.12/credentials/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/feature/ec2/imds` | https://github.com/aws/aws-sdk-go-v2/blob/feature/ec2/imds/v1.18.20/feature/ec2/imds/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/internal/configsources` | https://github.com/aws/aws-sdk-go-v2/blob/internal/configsources/v1.4.21/internal/configsources/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/internal/endpoints/v2` | https://github.com/aws/aws-sdk-go-v2/blob/internal/endpoints/v2.7.21/internal/endpoints/v2/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/internal/ini` | https://github.com/aws/aws-sdk-go-v2/blob/internal/ini/v1.8.6/internal/ini/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/service/bedrockruntime` | https://github.com/aws/aws-sdk-go-v2/blob/service/bedrockruntime/v1.50.4/service/bedrockruntime/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding` | https://github.com/aws/aws-sdk-go-v2/blob/service/internal/accept-encoding/v1.13.7/service/internal/accept-encoding/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/service/internal/presigned-url` | https://github.com/aws/aws-sdk-go-v2/blob/service/internal/presigned-url/v1.13.20/service/internal/presigned-url/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/service/signin` | https://github.com/aws/aws-sdk-go-v2/blob/service/signin/v1.0.8/service/signin/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/service/sso` | https://github.com/aws/aws-sdk-go-v2/blob/service/sso/v1.30.13/service/sso/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/service/ssooidc` | https://github.com/aws/aws-sdk-go-v2/blob/service/ssooidc/v1.35.17/service/ssooidc/LICENSE.txt |
| `github.com/aws/aws-sdk-go-v2/service/sts` | https://github.com/aws/aws-sdk-go-v2/blob/service/sts/v1.41.9/service/sts/LICENSE.txt |
| `github.com/aws/smithy-go` | https://github.com/aws/smithy-go/blob/v1.24.2/LICENSE |
| `github.com/containerd/stargz-snapshotter/estargz` | https://github.com/containerd/stargz-snapshotter/blob/estargz/v0.18.2/estargz/LICENSE |
| `github.com/docker/cli/cli/config` | https://github.com/docker/cli/blob/v29.4.0/LICENSE |
| `github.com/go-logr/logr` | https://github.com/go-logr/logr/blob/v1.4.4/LICENSE |
| `github.com/go-logr/stdr` | https://github.com/go-logr/stdr/blob/v1.2.2/LICENSE |
| `github.com/go-openapi/jsonpointer` | https://github.com/go-openapi/jsonpointer/blob/v0.22.5/LICENSE |
| `github.com/go-openapi/jsonreference` | https://github.com/go-openapi/jsonreference/blob/v0.21.5/LICENSE |
| `github.com/go-openapi/swag` | https://github.com/go-openapi/swag/blob/v0.25.5/LICENSE |
| `github.com/go-openapi/swag/cmdutils` | https://github.com/go-openapi/swag/blob/cmdutils/v0.25.5/cmdutils/LICENSE |
| `github.com/go-openapi/swag/conv` | https://github.com/go-openapi/swag/blob/conv/v0.25.5/conv/LICENSE |
| `github.com/go-openapi/swag/fileutils` | https://github.com/go-openapi/swag/blob/fileutils/v0.25.5/fileutils/LICENSE |
| `github.com/go-openapi/swag/jsonname` | https://github.com/go-openapi/swag/blob/jsonname/v0.25.5/jsonname/LICENSE |
| `github.com/go-openapi/swag/jsonutils` | https://github.com/go-openapi/swag/blob/jsonutils/v0.25.5/jsonutils/LICENSE |
| `github.com/go-openapi/swag/loading` | https://github.com/go-openapi/swag/blob/loading/v0.25.5/loading/LICENSE |
| `github.com/go-openapi/swag/mangling` | https://github.com/go-openapi/swag/blob/mangling/v0.25.5/mangling/LICENSE |
| `github.com/go-openapi/swag/netutils` | https://github.com/go-openapi/swag/blob/netutils/v0.25.5/netutils/LICENSE |
| `github.com/go-openapi/swag/stringutils` | https://github.com/go-openapi/swag/blob/stringutils/v0.25.5/stringutils/LICENSE |
| `github.com/go-openapi/swag/typeutils` | https://github.com/go-openapi/swag/blob/typeutils/v0.25.5/typeutils/LICENSE |
| `github.com/go-openapi/swag/yamlutils` | https://github.com/go-openapi/swag/blob/yamlutils/v0.25.5/yamlutils/LICENSE |
| `github.com/google/cel-go` | https://github.com/google/cel-go/blob/v0.30.0/LICENSE |
| `github.com/google/gnostic-models` | https://github.com/google/gnostic-models/blob/v0.7.1/LICENSE |
| `github.com/google/go-containerregistry` | https://github.com/google/go-containerregistry/blob/v0.21.5/LICENSE |
| `github.com/google/s2a-go` | https://github.com/google/s2a-go/blob/v0.1.9/LICENSE.md |
| `github.com/googleapis/enterprise-certificate-proxy/client` | https://github.com/googleapis/enterprise-certificate-proxy/blob/v0.3.14/LICENSE |
| `github.com/klauspost/compress` | https://github.com/klauspost/compress/blob/v1.19.1/LICENSE |
| `github.com/kylelemons/godebug` | https://github.com/kylelemons/godebug/blob/v1.1.0/LICENSE |
| `github.com/kyverno/api/api/policies.kyverno.io` | https://github.com/kyverno/api/blob/7b64bcf2b1f7/LICENSE |
| `github.com/kyverno/pkg/certmanager` | https://github.com/kyverno/pkg/blob/456016242e005e2ac960de917a8aa260b717c8e6/LICENSE |
| `github.com/kyverno/pkg/tls` | https://github.com/kyverno/pkg/blob/456016242e005e2ac960de917a8aa260b717c8e6/LICENSE |
| `github.com/kyverno/sdk/extensions` | https://github.com/kyverno/sdk/blob/7ad4d9dcec7e/LICENSE |
| `github.com/moby/spdystream` | https://github.com/moby/spdystream/blob/v0.5.1/LICENSE |
| `github.com/modern-go/concurrent` | https://github.com/modern-go/concurrent/blob/bacd9c7ef1dd/LICENSE |
| `github.com/modern-go/reflect2` | https://github.com/modern-go/reflect2/blob/35a7c28c31ee/LICENSE |
| `github.com/openai/openai-go` | https://github.com/openai/openai-go/blob/v1.12.0/LICENSE |
| `github.com/opencontainers/go-digest` | https://github.com/opencontainers/go-digest/blob/v1.0.0/LICENSE |
| `github.com/opencontainers/image-spec/specs-go` | https://github.com/opencontainers/image-spec/blob/v1.1.1/LICENSE |
| `github.com/prometheus/client_golang` | https://github.com/prometheus/client_golang/blob/v1.24.1/LICENSE |
| `github.com/prometheus/client_model/go` | https://github.com/prometheus/client_model/blob/v0.6.2/LICENSE |
| `github.com/prometheus/common` | https://github.com/prometheus/common/blob/v0.70.1/LICENSE |
| `github.com/prometheus/procfs` | https://github.com/prometheus/procfs/blob/v0.21.1/LICENSE |
| `github.com/santhosh-tekuri/jsonschema/v6` | https://github.com/santhosh-tekuri/jsonschema/blob/v6.0.2/LICENSE |
| `github.com/spf13/cobra` | https://github.com/spf13/cobra/blob/v1.10.2/LICENSE.txt |
| `github.com/zach-klippenstein/goregen` | https://github.com/zach-klippenstein/goregen/blob/795b5e3961ea/LICENSE.txt |
| `go.opentelemetry.io/auto/sdk` | https://github.com/open-telemetry/opentelemetry-go-instrumentation/blob/sdk/v1.2.1/sdk/LICENSE |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | https://github.com/open-telemetry/opentelemetry-go-contrib/blob/instrumentation/net/http/otelhttp/v0.68.0/instrumentation/net/http/otelhttp/LICENSE |
| `go.opentelemetry.io/otel` | https://github.com/open-telemetry/opentelemetry-go/blob/v1.44.0/LICENSE |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace` | https://github.com/open-telemetry/opentelemetry-go/blob/exporters/otlp/otlptrace/v1.44.0/exporters/otlp/otlptrace/LICENSE |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` | https://github.com/open-telemetry/opentelemetry-go/blob/exporters/otlp/otlptrace/otlptracegrpc/v1.44.0/exporters/otlp/otlptrace/otlptracegrpc/LICENSE |
| `go.opentelemetry.io/otel/metric` | https://github.com/open-telemetry/opentelemetry-go/blob/metric/v1.44.0/metric/LICENSE |
| `go.opentelemetry.io/otel/sdk` | https://github.com/open-telemetry/opentelemetry-go/blob/sdk/v1.44.0/sdk/LICENSE |
| `go.opentelemetry.io/otel/trace` | https://github.com/open-telemetry/opentelemetry-go/blob/trace/v1.44.0/trace/LICENSE |
| `go.opentelemetry.io/proto/otlp` | https://github.com/open-telemetry/opentelemetry-proto-go/blob/otlp/v1.10.0/otlp/LICENSE |
| `go.yaml.in/yaml/v2` | https://github.com/yaml/go-yaml/blob/v2.4.4/LICENSE |
| `gomodules.xyz/jsonpatch/v2` | https://github.com/gomodules/jsonpatch/blob/v2.5.0/v2/LICENSE |
| `google.golang.org/genai` | https://github.com/googleapis/go-genai/blob/v1.8.0/LICENSE |
| `google.golang.org/genproto/googleapis/api` | https://github.com/googleapis/go-genproto/blob/3dc84a4a5aaa/googleapis/api/LICENSE |
| `google.golang.org/genproto/googleapis/rpc` | https://github.com/googleapis/go-genproto/blob/3dc84a4a5aaa/googleapis/rpc/LICENSE |
| `google.golang.org/grpc` | https://github.com/grpc/grpc-go/blob/v1.82.1/LICENSE |
| `k8s.io/api` | https://github.com/kubernetes/api/blob/v0.36.3/LICENSE |
| `k8s.io/apiextensions-apiserver/pkg/apis/apiextensions` | https://github.com/kubernetes/apiextensions-apiserver/blob/v0.36.3/LICENSE |
| `k8s.io/apimachinery/pkg` | https://github.com/kubernetes/apimachinery/blob/v0.36.3/LICENSE |
| `k8s.io/apiserver/pkg` | https://github.com/kubernetes/apiserver/blob/v0.36.3/LICENSE |
| `k8s.io/client-go` | https://github.com/kubernetes/client-go/blob/v0.36.3/LICENSE |
| `k8s.io/component-base` | https://github.com/kubernetes/component-base/blob/v0.36.3/LICENSE |
| `k8s.io/klog/v2` | https://github.com/kubernetes/klog/blob/v2.140.0/LICENSE |
| `k8s.io/kube-openapi/pkg` | https://github.com/kubernetes/kube-openapi/blob/43fb72c5454a/LICENSE |
| `k8s.io/kube-openapi/pkg/validation/errors` | https://github.com/kubernetes/kube-openapi/blob/43fb72c5454a/pkg/validation/errors/LICENSE |
| `k8s.io/kube-openapi/pkg/validation/spec` | https://github.com/kubernetes/kube-openapi/blob/43fb72c5454a/pkg/validation/spec/LICENSE |
| `k8s.io/kube-openapi/pkg/validation/strfmt` | https://github.com/kubernetes/kube-openapi/blob/43fb72c5454a/pkg/validation/strfmt/LICENSE |
| `k8s.io/metrics/pkg` | https://github.com/kubernetes/metrics/blob/v0.36.3/LICENSE |
| `k8s.io/streaming/pkg` | https://github.com/kubernetes/streaming/blob/v0.36.3/LICENSE |
| `k8s.io/utils` | https://github.com/kubernetes/utils/blob/b8788abfbbc2/LICENSE |
| `k8s.io/utils/third_party/forked/golang/btree` | https://github.com/kubernetes/utils/blob/b8788abfbbc2/third_party/forked/golang/btree/LICENSE |
| `sigs.k8s.io/controller-runtime` | https://github.com/kubernetes-sigs/controller-runtime/blob/v0.24.1/LICENSE |
| `sigs.k8s.io/json` | https://github.com/kubernetes-sigs/json/blob/2d320260d730/LICENSE |
| `sigs.k8s.io/randfill` | https://github.com/kubernetes-sigs/randfill/blob/v1.0.0/LICENSE |
| `sigs.k8s.io/structured-merge-diff/v6` | https://github.com/kubernetes-sigs/structured-merge-diff/blob/v6.3.3/LICENSE |
| `sigs.k8s.io/yaml` | https://github.com/kubernetes-sigs/yaml/blob/v1.6.0/LICENSE |

## MIT

| Dependency | Source |
|---|---|
| `github.com/Azure/azure-sdk-for-go/sdk/ai/azopenai` | https://github.com/Azure/azure-sdk-for-go/blob/sdk/ai/azopenai/v0.7.2/sdk/ai/azopenai/LICENSE.txt |
| `github.com/Azure/azure-sdk-for-go/sdk/azcore` | https://github.com/Azure/azure-sdk-for-go/blob/sdk/azcore/v1.21.0/sdk/azcore/LICENSE.txt |
| `github.com/Azure/azure-sdk-for-go/sdk/azidentity` | https://github.com/Azure/azure-sdk-for-go/blob/sdk/azidentity/v1.13.1/sdk/azidentity/LICENSE.txt |
| `github.com/Azure/azure-sdk-for-go/sdk/internal` | https://github.com/Azure/azure-sdk-for-go/blob/sdk/internal/v1.11.2/sdk/internal/LICENSE.txt |
| `github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/cognitiveservices/armcognitiveservices` | https://github.com/Azure/azure-sdk-for-go/blob/sdk/resourcemanager/cognitiveservices/armcognitiveservices/v1.7.0/sdk/resourcemanager/cognitiveservices/armcognitiveservices/LICENSE.txt |
| `github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/subscription/armsubscription` | https://github.com/Azure/azure-sdk-for-go/blob/sdk/resourcemanager/subscription/armsubscription/v1.2.0/sdk/resourcemanager/subscription/armsubscription/LICENSE.txt |
| `github.com/AzureAD/microsoft-authentication-library-for-go/apps` | https://github.com/AzureAD/microsoft-authentication-library-for-go/blob/v1.6.0/LICENSE |
| `github.com/Masterminds/semver/v3` | https://github.com/Masterminds/semver/blob/v3.4.0/LICENSE.txt |
| `github.com/alecthomas/chroma/v2` | https://github.com/alecthomas/chroma/blob/v2.20.0/COPYING |
| `github.com/anthropics/anthropic-sdk-go` | https://github.com/anthropics/anthropic-sdk-go/blob/v1.26.0/LICENSE |
| `github.com/aptible/supercronic/cronexpr` | https://github.com/aptible/supercronic/blob/v0.2.43/LICENSE.md |
| `github.com/aymanbagabas/go-osc52/v2` | https://github.com/aymanbagabas/go-osc52/blob/v2.0.1/LICENSE |
| `github.com/aymerick/douceur` | https://github.com/aymerick/douceur/blob/v0.2.0/LICENSE |
| `github.com/beorn7/perks/quantile` | https://github.com/beorn7/perks/blob/v1.0.1/LICENSE |
| `github.com/blang/semver/v4` | https://github.com/blang/semver/blob/v4.0.0/v4/LICENSE |
| `github.com/cenkalti/backoff/v5` | https://github.com/cenkalti/backoff/blob/v5.0.3/LICENSE |
| `github.com/cespare/xxhash/v2` | https://github.com/cespare/xxhash/blob/v2.3.0/LICENSE.txt |
| `github.com/charmbracelet/colorprofile` | https://github.com/charmbracelet/colorprofile/blob/f60798e515dc/LICENSE |
| `github.com/charmbracelet/glamour` | https://github.com/charmbracelet/glamour/blob/v1.0.0/LICENSE |
| `github.com/charmbracelet/lipgloss` | https://github.com/charmbracelet/lipgloss/blob/76690c660834/LICENSE |
| `github.com/charmbracelet/x/ansi` | https://github.com/charmbracelet/x/blob/ansi/v0.10.2/ansi/LICENSE |
| `github.com/charmbracelet/x/cellbuf` | https://github.com/charmbracelet/x/blob/cellbuf/v0.0.13/cellbuf/LICENSE |
| `github.com/charmbracelet/x/exp/slice` | https://github.com/charmbracelet/x/blob/2fdc97757edf/exp/slice/LICENSE |
| `github.com/charmbracelet/x/term` | https://github.com/charmbracelet/x/blob/term/v0.2.1/term/LICENSE |
| `github.com/clipperhouse/uax29/v2/graphemes` | https://github.com/clipperhouse/uax29/blob/v2.6.0/LICENSE |
| `github.com/dlclark/regexp2` | https://github.com/dlclark/regexp2/blob/v1.11.5/LICENSE |
| `github.com/docker/docker-credential-helpers` | https://github.com/docker/docker-credential-helpers/blob/v0.9.5/LICENSE |
| `github.com/emicklei/go-restful/v3` | https://github.com/emicklei/go-restful/blob/v3.13.0/LICENSE |
| `github.com/felixge/httpsnoop` | https://github.com/felixge/httpsnoop/blob/v1.0.4/LICENSE.txt |
| `github.com/fxamacker/cbor/v2` | https://github.com/fxamacker/cbor/blob/v2.9.0/LICENSE |
| `github.com/golang-jwt/jwt/v5` | https://github.com/golang-jwt/jwt/blob/v5.3.1/LICENSE |
| `github.com/google/jsonschema-go/jsonschema` | https://github.com/google/jsonschema-go/blob/v0.4.2/LICENSE |
| `github.com/json-iterator/go` | https://github.com/json-iterator/go/blob/v1.1.12/LICENSE |
| `github.com/klauspost/compress/zstd/internal/xxhash` | https://github.com/klauspost/compress/blob/v1.19.1/zstd/internal/xxhash/LICENSE.txt |
| `github.com/lucasb-eyer/go-colorful` | https://github.com/lucasb-eyer/go-colorful/blob/v1.3.0/LICENSE |
| `github.com/mark3labs/mcp-go` | https://github.com/mark3labs/mcp-go/blob/v1.4.0/LICENSE |
| `github.com/mattn/go-isatty` | https://github.com/mattn/go-isatty/blob/v0.0.20/LICENSE |
| `github.com/mattn/go-runewidth` | https://github.com/mattn/go-runewidth/blob/v0.0.19/LICENSE |
| `github.com/mitchellh/go-homedir` | https://github.com/mitchellh/go-homedir/blob/v1.1.0/LICENSE |
| `github.com/muesli/reflow` | https://github.com/muesli/reflow/blob/v0.3.0/LICENSE |
| `github.com/muesli/termenv` | https://github.com/muesli/termenv/blob/v0.16.0/LICENSE |
| `github.com/ollama/ollama` | https://github.com/ollama/ollama/blob/v0.6.5/LICENSE |
| `github.com/onsi/ginkgo/v2` | https://github.com/onsi/ginkgo/blob/v2.32.0/LICENSE |
| `github.com/rivo/uniseg` | https://github.com/rivo/uniseg/blob/v0.4.7/LICENSE.txt |
| `github.com/robfig/cron/v3` | https://github.com/robfig/cron/blob/v3.0.1/LICENSE |
| `github.com/sirupsen/logrus` | https://github.com/sirupsen/logrus/blob/v1.9.4/LICENSE |
| `github.com/spf13/cast` | https://github.com/spf13/cast/blob/v1.10.0/LICENSE |
| `github.com/tidwall/gjson` | https://github.com/tidwall/gjson/blob/v1.18.0/LICENSE |
| `github.com/tidwall/match` | https://github.com/tidwall/match/blob/v1.1.1/LICENSE |
| `github.com/tidwall/pretty` | https://github.com/tidwall/pretty/blob/v1.2.1/LICENSE |
| `github.com/tidwall/sjson` | https://github.com/tidwall/sjson/blob/v1.2.5/LICENSE |
| `github.com/x448/float16` | https://github.com/x448/float16/blob/v0.8.4/LICENSE |
| `github.com/xo/terminfo` | https://github.com/xo/terminfo/blob/abceb7e1c41e/LICENSE |
| `github.com/yuin/goldmark` | https://github.com/yuin/goldmark/blob/v1.7.13/LICENSE |
| `github.com/yuin/goldmark-emoji` | https://github.com/yuin/goldmark-emoji/blob/v1.0.6/LICENSE |
| `go.yaml.in/yaml/v3` | https://github.com/yaml/go-yaml/blob/v3.0.4/LICENSE |
| `k8s.io/kube-openapi/pkg/internal/third_party/govalidator` | https://github.com/kubernetes/kube-openapi/blob/43fb72c5454a/pkg/internal/third_party/govalidator/LICENSE |

## BSD-3-Clause

| Dependency | Source |
|---|---|
| `github.com/antlr4-go/antlr/v4` | https://github.com/antlr4-go/antlr/blob/v4.13.1/LICENSE |
| `github.com/aws/aws-sdk-go-v2/internal/sync/singleflight` | https://github.com/aws/aws-sdk-go-v2/blob/v1.41.5/internal/sync/singleflight/LICENSE |
| `github.com/aws/smithy-go/internal/sync/singleflight` | https://github.com/aws/smithy-go/blob/v1.24.2/internal/sync/singleflight/LICENSE |
| `github.com/evanphx/json-patch/v5` | https://github.com/evanphx/json-patch/blob/v5.9.11/v5/LICENSE |
| `github.com/fsnotify/fsnotify` | https://github.com/fsnotify/fsnotify/blob/v1.9.0/LICENSE |
| `github.com/google/go-cmp/cmp` | https://github.com/google/go-cmp/blob/v0.7.0/LICENSE |
| `github.com/google/uuid` | https://github.com/google/uuid/blob/v1.6.0/LICENSE |
| `github.com/googleapis/gax-go/v2/internallog` | https://github.com/googleapis/gax-go/blob/v2.19.0/v2/LICENSE |
| `github.com/gorilla/css/scanner` | https://github.com/gorilla/css/blob/v1.0.1/LICENSE |
| `github.com/grpc-ecosystem/grpc-gateway/v2` | https://github.com/grpc-ecosystem/grpc-gateway/blob/v2.29.0/LICENSE |
| `github.com/klauspost/compress/internal/snapref` | https://github.com/klauspost/compress/blob/v1.19.1/internal/snapref/LICENSE |
| `github.com/microcosm-cc/bluemonday` | https://github.com/microcosm-cc/bluemonday/blob/v1.0.27/LICENSE.md |
| `github.com/moby/spdystream/spdy` | https://github.com/moby/spdystream/blob/v0.5.1/spdy/LICENSE |
| `github.com/munnerz/goautoneg` | https://github.com/munnerz/goautoneg/blob/a7dc8b61c822/LICENSE |
| `github.com/pmezard/go-difflib/difflib` | https://github.com/pmezard/go-difflib/blob/5d4384ee4fb2/LICENSE |
| `github.com/prometheus/client_golang/internal/github.com/golang/gddo/httputil` | https://github.com/prometheus/client_golang/blob/v1.24.1/internal/github.com/golang/gddo/LICENSE |
| `github.com/spf13/pflag` | https://github.com/spf13/pflag/blob/v1.0.10/LICENSE |
| `github.com/vbatts/tar-split/archive/tar` | https://github.com/vbatts/tar-split/blob/v0.12.2/LICENSE |
| `github.com/yosida95/uritemplate/v3` | https://github.com/yosida95/uritemplate/blob/v3.0.2/LICENSE |
| `golang.org/x/crypto` | https://cs.opensource.google/go/x/crypto/+/v0.54.0:LICENSE |
| `golang.org/x/exp/slices` | https://cs.opensource.google/go/x/exp/+/3dfff04d:LICENSE |
| `golang.org/x/mod/semver` | https://cs.opensource.google/go/x/mod/+/v0.37.0:LICENSE |
| `golang.org/x/net` | https://cs.opensource.google/go/x/net/+/v0.57.0:LICENSE |
| `golang.org/x/oauth2` | https://cs.opensource.google/go/x/oauth2/+/v0.36.0:LICENSE |
| `golang.org/x/sync` | https://cs.opensource.google/go/x/sync/+/v0.22.0:LICENSE |
| `golang.org/x/sys/unix` | https://cs.opensource.google/go/x/sys/+/v0.47.0:LICENSE |
| `golang.org/x/term` | https://cs.opensource.google/go/x/term/+/v0.45.0:LICENSE |
| `golang.org/x/text` | https://cs.opensource.google/go/x/text/+/v0.40.0:LICENSE |
| `golang.org/x/time/rate` | https://cs.opensource.google/go/x/time/+/v0.15.0:LICENSE |
| `golang.org/x/tools` | https://cs.opensource.google/go/x/tools/+/v0.47.0:LICENSE |
| `google.golang.org/protobuf` | https://github.com/protocolbuffers/protobuf-go/blob/f2248ac996af/LICENSE |
| `gopkg.in/evanphx/json-patch.v4` | https://github.com/evanphx/json-patch/blob/v4.13.0/LICENSE |
| `gopkg.in/inf.v0` | https://github.com/go-inf/inf/blob/v0.9.1/LICENSE |
| `k8s.io/apimachinery/third_party/forked/golang` | https://github.com/kubernetes/apimachinery/blob/v0.36.3/third_party/forked/golang/LICENSE |
| `k8s.io/kube-openapi/pkg/internal/third_party/go-json-experiment/json` | https://github.com/kubernetes/kube-openapi/blob/43fb72c5454a/pkg/internal/third_party/go-json-experiment/json/LICENSE |
| `k8s.io/utils/internal/third_party/forked/golang` | https://github.com/kubernetes/utils/blob/b8788abfbbc2/internal/third_party/forked/golang/LICENSE |
| `mvdan.cc/sh/v3` | https://github.com/mvdan/sh/blob/v3.11.0/LICENSE |

## BSD-2-Clause

| Dependency | Source |
|---|---|
| `github.com/gorilla/websocket` | https://github.com/gorilla/websocket/blob/e064f32e3674/LICENSE |
| `github.com/pkg/browser` | https://github.com/pkg/browser/blob/5ac0b6a4141c/LICENSE |

## MPL-2.0

| Dependency | Source |
|---|---|
| `github.com/hashicorp/golang-lru/v2` | https://github.com/hashicorp/golang-lru/blob/v2.0.7/LICENSE |

## ISC

| Dependency | Source |
|---|---|
| `github.com/davecgh/go-spew/spew` | https://github.com/davecgh/go-spew/blob/d8f796af33cc/LICENSE |

## Regenerating

Run `make licenses` from the repo root.
