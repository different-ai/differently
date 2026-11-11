// Provider kinds the console can add, with their main config fields: the kinds
// in the default server build. Experimental kinds (a server built with
// -tags experimental_adapters) are added with the CLI, the API or the config
// file. Kept in step with the built-in dashboard
// (internal/dashboard/static/app.js); the full list of keys per provider is in
// docs/providers/.
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
  }
];
