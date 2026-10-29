// Provider kinds the console can add, with their main config fields. Kept in
// step with the built-in dashboard (internal/dashboard/static/app.js); the full
// list of keys per provider is in docs/providers/.
export interface ProviderField { key: string; label: string; choices?: string[]; required?: boolean }
export interface ProviderKind { kind: string; label: string; trust: "external" | null; fields: ProviderField[] }

export const providerKinds: ProviderKind[] = [
  {
    "kind": "daytona",
    "label": "Daytona",
    "trust": "external",
    "fields": [
      {
        "key": "apiURL",
        "label": "API URL (default https://app.daytona.io/api)"
      },
      {
        "key": "target",
        "label": "target region"
      },
      {
        "key": "snapshot",
        "label": "snapshot"
      },
      {
        "key": "class",
        "label": "class",
        "choices": [
          "container",
          "linux-vm"
        ]
      }
    ]
  },
  {
    "kind": "freestyle",
    "label": "Freestyle",
    "trust": "external",
    "fields": [
      {
        "key": "apiURL",
        "label": "API URL (default https://api.freestyle.sh)"
      },
      {
        "key": "snapshot",
        "label": "snapshot"
      },
      {
        "key": "egress",
        "label": "egress",
        "choices": [
          "none",
          "public"
        ]
      },
      {
        "key": "domain",
        "label": "domain (default style.dev)"
      }
    ]
  },
  {
    "kind": "boat",
    "label": "Boat",
    "trust": "external",
    "fields": [
      {
        "key": "apiURL",
        "label": "API URL (default https://boat.dev/api/v1)"
      },
      {
        "key": "type",
        "label": "smallest size",
        "choices": [
          "small",
          "default",
          "large",
          "xlarge"
        ]
      },
      {
        "key": "snapshot",
        "label": "saved snapshot"
      },
      {
        "key": "environment",
        "label": "environment"
      },
      {
        "key": "org",
        "label": "organization"
      },
      {
        "key": "autoStopSeconds",
        "label": "auto-stop seconds for sandboxes without a TTL"
      },
      {
        "key": "portAccess",
        "label": "port URLs",
        "choices": [
          "token",
          "public"
        ]
      }
    ]
  },
  {
    "kind": "e2b",
    "label": "E2B",
    "trust": null,
    "fields": [
      {
        "key": "apiURL",
        "label": "API URL (default https://api.e2b.app)"
      },
      {
        "key": "domain",
        "label": "domain (default e2b.app)"
      },
      {
        "key": "template",
        "label": "template (default base)"
      },
      {
        "key": "maxLifetimeSeconds",
        "label": "plan's maximum runtime in seconds (default 3600)"
      },
      {
        "key": "ports",
        "label": "ports",
        "choices": [
          "private",
          "public"
        ]
      },
      {
        "key": "egress",
        "label": "egress",
        "choices": [
          "public",
          "none"
        ]
      }
    ]
  },
  {
    "kind": "morph",
    "label": "Morph Cloud",
    "trust": "external",
    "fields": [
      {
        "key": "apiURL",
        "label": "API URL (default https://cloud.morph.so/api)"
      },
      {
        "key": "snapshot",
        "label": "snapshot"
      },
      {
        "key": "image",
        "label": "image (default morphvm-minimal)"
      },
      {
        "key": "portAuth",
        "label": "port auth",
        "choices": [
          "none",
          "api_key"
        ]
      }
    ]
  },
  {
    "kind": "blaxel",
    "label": "Blaxel",
    "trust": "external",
    "fields": [
      {
        "key": "workspace",
        "label": "workspace",
        "required": true
      },
      {
        "key": "apiURL",
        "label": "API URL (default https://api.blaxel.ai/v0)"
      },
      {
        "key": "region",
        "label": "region (e.g. us-pdx-1)"
      },
      {
        "key": "image",
        "label": "image (default blaxel/base-image:latest)"
      }
    ]
  },
  {
    "kind": "opensandbox",
    "label": "OpenSandbox",
    "trust": null,
    "fields": [
      {
        "key": "apiURL",
        "label": "lifecycle API URL",
        "required": true
      },
      {
        "key": "image",
        "label": "image"
      },
      {
        "key": "templateId",
        "label": "template id"
      },
      {
        "key": "isolation",
        "label": "isolation",
        "choices": [
          "container",
          "microvm"
        ]
      },
      {
        "key": "runtime",
        "label": "runtime",
        "choices": [
          "docker",
          "kubernetes"
        ]
      },
      {
        "key": "lineEndings",
        "label": "line endings",
        "choices": [
          "kept",
          "stripped"
        ]
      },
      {
        "key": "ports",
        "label": "port URLs",
        "choices": [
          "signed"
        ]
      },
      {
        "key": "egress",
        "label": "egress",
        "choices": [
          "none",
          "public",
          "allowlist"
        ]
      },
      {
        "key": "egressAllow",
        "label": "egress allowlist (comma-separated hosts)"
      },
      {
        "key": "useServerProxy",
        "label": "server proxy",
        "choices": [
          "true",
          "false"
        ]
      }
    ]
  },
  {
    "kind": "remote",
    "label": "Remote plugin",
    "trust": null,
    "fields": [
      {
        "key": "endpoint",
        "label": "plugin endpoint URL",
        "required": true
      }
    ]
  },
  {
    "kind": "vercel",
    "label": "Vercel Sandbox",
    "trust": "external",
    "fields": [
      {
        "key": "project",
        "label": "project ID or name",
        "required": true
      },
      {
        "key": "team",
        "label": "team ID"
      },
      {
        "key": "region",
        "label": "region (default iad1)"
      },
      {
        "key": "image",
        "label": "image"
      },
      {
        "key": "architecture",
        "label": "architecture",
        "choices": [
          "amd64",
          "arm64"
        ]
      },
      {
        "key": "ports",
        "label": "ports (comma separated)"
      },
      {
        "key": "maxTimeoutSeconds",
        "label": "max session seconds (default 2700)"
      }
    ]
  },
  {
    "kind": "fly",
    "label": "Fly.io Machines",
    "trust": "external",
    "fields": [
      {
        "key": "app",
        "label": "Fly app name",
        "required": true
      },
      {
        "key": "region",
        "label": "region"
      },
      {
        "key": "image",
        "label": "image"
      },
      {
        "key": "cpuKind",
        "label": "CPU kind",
        "choices": [
          "shared",
          "performance"
        ]
      },
      {
        "key": "suspend",
        "label": "suspend for pause",
        "choices": [
          "true",
          "false"
        ]
      }
    ]
  },
  {
    "kind": "agent-sandbox",
    "label": "agent-sandbox (Kubernetes)",
    "trust": null,
    "fields": [
      {
        "key": "namespace",
        "label": "namespace (default default)"
      },
      {
        "key": "apiServer",
        "label": "API server URL (default: in-cluster)"
      },
      {
        "key": "caFile",
        "label": "API server CA file on the server"
      },
      {
        "key": "image",
        "label": "image (default ubuntu:24.04)"
      },
      {
        "key": "warmPool",
        "label": "warm pool (SandboxWarmPool name)"
      },
      {
        "key": "runtimeClassName",
        "label": "RuntimeClass (e.g. gvisor)"
      },
      {
        "key": "isolation",
        "label": "isolation",
        "choices": [
          "container",
          "microvm"
        ]
      },
      {
        "key": "volumeSize",
        "label": "volume size, e.g. 10Gi (enables stop/start)"
      }
    ]
  },
  {
    "kind": "cubesandbox",
    "label": "CubeSandbox",
    "trust": null,
    "fields": [
      {
        "key": "apiURL",
        "label": "CubeAPI URL",
        "required": true
      },
      {
        "key": "templateId",
        "label": "template id"
      },
      {
        "key": "sandboxURL",
        "label": "CubeProxy URL (header routing)"
      },
      {
        "key": "domain",
        "label": "sandbox domain (default: CubeAPI's)"
      },
      {
        "key": "proxyScheme",
        "label": "proxy scheme",
        "choices": [
          "https",
          "http"
        ]
      }
    ]
  },
  {
    "kind": "aws-agentcore",
    "label": "AWS AgentCore Code Interpreter",
    "trust": null,
    "fields": [
      {
        "key": "region",
        "label": "AWS region",
        "required": true
      },
      {
        "key": "codeInterpreter",
        "label": "code interpreter (default aws.codeinterpreter.v1)"
      }
    ]
  },
  {
    "kind": "azure-sandboxes",
    "label": "Azure Container Apps Sandboxes",
    "trust": null,
    "fields": [
      {
        "key": "endpoint",
        "label": "data-plane endpoint",
        "required": true
      },
      {
        "key": "subscriptionId",
        "label": "subscription ID",
        "required": true
      },
      {
        "key": "resourceGroup",
        "label": "resource group",
        "required": true
      },
      {
        "key": "sandboxGroup",
        "label": "sandbox group",
        "required": true
      },
      {
        "key": "diskImage",
        "label": "public disk image"
      },
      {
        "key": "egress",
        "label": "egress",
        "choices": [
          "none",
          "public"
        ]
      }
    ]
  },
  {
    "kind": "azure-sessions",
    "label": "Azure dynamic sessions (Python)",
    "trust": null,
    "fields": [
      {
        "key": "endpoint",
        "label": "pool management endpoint",
        "required": true
      },
      {
        "key": "outputLimit",
        "label": "output limit (default 1048576)"
      }
    ]
  },
  {
    "kind": "gcp-sandboxes",
    "label": "Google Cloud Agent Platform sandboxes",
    "trust": null,
    "fields": [
      {
        "key": "project",
        "label": "project ID",
        "required": true
      },
      {
        "key": "location",
        "label": "location",
        "required": true
      },
      {
        "key": "reasoningEngine",
        "label": "engine ID",
        "required": true
      },
      {
        "key": "template",
        "label": "shell template ID",
        "required": true
      }
    ]
  },
  {
    "kind": "northflank",
    "label": "Northflank",
    "trust": null,
    "fields": [
      {
        "key": "projectId",
        "label": "project ID",
        "required": true
      },
      {
        "key": "teamId",
        "label": "team ID (organization tokens)"
      },
      {
        "key": "image",
        "label": "image (default ubuntu:24.04)"
      },
      {
        "key": "plan",
        "label": "plan (default nf-compute-200)"
      }
    ]
  }
];
