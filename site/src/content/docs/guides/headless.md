---
title: Run it on a cluster
description: Run the daemon from its container image on Kubernetes, ECS, Nomad or plain Docker, with every interactive sign-in replaced by a credential the orchestrator supplies.
---

Each release publishes an image of the daemon, `ghcr.io/open-conduit/open-conduit`. A deployment needs that image, one volume, a config file and the credentials below, and no login shell. This guide covers what the image holds, what replaces each interactive step of the [team guide](../team/), and what an orchestrator has to respect. A Kubernetes example comes [at the end](#a-kubernetes-example).

## The image

| | |
| --- | --- |
| Tags | `X.Y.Z` for each release, `X.Y` for the newest patch of a minor release, and `latest`. Pin `X.Y.Z` or a digest. |
| Platforms | `linux/amd64` and `linux/arm64` |
| Contents | `open-conduit`, `os` and the bundled extension packages from the release archive, in `/opt/open-conduit`, plus git, an SSH client, Node 24 (for TypeScript packages) and a pinned Claude Code (`claude --version` shows which). |
| User | `conduit`, uid and gid `10001`, never root |
| Volume | `/home/conduit`, the user's home. It holds the state (`~/.local/state/open-conduit`), the config directory (`~/.config/open-conduit`, with any packages you install) and Claude Code's own config directory (`~/.claude`, where its sessions are kept). |
| Entry point | `open-conduit serve --no-updates` with whatever flags you pass. A first argument that isn't a flag runs that command instead, with the mounted config installed first, as in `docker run … ghcr.io/open-conduit/open-conduit config check`. |

`--no-updates` turns off the web view's update page and anything else that would install a release from inside the container. To upgrade, roll out the next image tag. The daemon in the old container stops, and the new one adopts its state.

The image doesn't include Codex, other agents' CLIs or your repositories' toolchains. Build your own image `FROM` this one and add them:

```dockerfile
FROM ghcr.io/open-conduit/open-conduit:X.Y.Z
USER root
RUN npm install -g @openai/codex    # pin the version you have tested
USER 10001:10001
```

## Try it with Docker

```sh
docker volume create open-conduit
docker run -d --name open-conduit \
  -v open-conduit:/home/conduit \
  -v "$PWD/config.json:/etc/open-conduit/config.json:ro" \
  -e CLAUDE_TOKEN -e GITHUB_APP_KEY \
  -p 8787:8787 \
  ghcr.io/open-conduit/open-conduit:X.Y.Z
docker exec open-conduit open-conduit doctor
```

`docker exec` runs the CLI as the daemon's own user, against its socket, as `operator:local`. Use it for `doctor`, `status`, `secret`, `auth add` and the other operator commands.

## Config and secrets

The daemon reads its config and `secrets.json` only from regular, owner-only (`0600`) files that its own user owns. A Kubernetes ConfigMap or Secret mount, a Docker config or a bind mount from the host usually isn't one. So mount them in `/etc/open-conduit` instead (or the directory `OPEN_CONDUIT_CONFIG_SOURCE` names). At every start, the entry point copies `/etc/open-conduit/config.json` and `/etc/open-conduit/secrets.json`, when they are there, over the daemon's own files with the right mode. The mount is the source of truth. A change made inside the container, with `config set` or `secret set`, lasts only until the next start, so make changes in the mounted file. When you take a file out of the mount, the entry point removes the copy it installed at the next start, so a secret you removed doesn't stay behind on the volume. A file the entry point didn't install is left alone.

A changed mount takes effect when the container restarts (`kubectl rollout restart statefulset/open-conduit`, say).

Without a keyring, a secret reference is one of these:

- **`env:NAME`**, an environment variable of the daemon. This is the one to use with an orchestrator: put the value in its secret store (a Kubernetes Secret, an ECS task's `secrets`, Nomad's `template`) and give it to the container as a variable. A multi-line value such as a private key works.
- **`file:NAME`**, the key `NAME` in `secrets.json`, a JSON object of strings.
- **`cmd:COMMAND`**, a command's output, for a secret manager's CLI you add to the image.
- **`aws-sm:…`**, an AWS Secrets Manager secret read by the bundled `aws` extension with the daemon's own AWS identity ([AWS extension](/open-conduit/architecture/aws-extension/)).

`keyring:` references don't work in the image, because there is no keyring.

## Replace each interactive sign-in

| On a login account | In the image |
| --- | --- |
| `claude` and `/login` | A token from `claude setup-token`, or an API key, as a secret the binding maps to `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` in `secret_env`. Or a gateway with the binding's `provider` ([other providers](../third-party-models/)), or [Bedrock or Vertex AI](#claude-code-on-bedrock-or-vertex-ai). |
| `gh auth login` | A GitHub App connection, `"auth": "app"`, with its private key as `app_private_key`. git pushes through the toolbelt with a token minted for the Work's one repository, so neither `gh` nor a credential helper is needed ([GitHub bot](../github-bot/)). |
| The keyring | `env:` or `file:` references, as above. |
| `service install` | The orchestrator, which restarts the container when the daemon exits with an error. |
| `open-conduit upgrade` | A new image tag. |

The Claude Code binding selects the token as one of its scope's secrets:

```json
{"id": "web", "name": "acme/web",
 "secret_refs": {"claude-token": "env:CLAUDE_TOKEN"},
 "bindings": [{"name": "agent", "executor": "claude-code", "connection": "github",
   "secret_names": ["claude-token"],
   "config": {"playbook": "work-issue", "secret_env": {"claude-token": "CLAUDE_CODE_OAUTH_TOKEN"}}}]}
```

and the GitHub connection signs in as the App:

```json
{"id": "github", "type": "github", "config": {"auth": "app", "app_id": "123456"},
 "secret_refs": {"app_private_key": "env:GITHUB_APP_KEY"}}
```

Never bake a personal login into an image: a `~/.claude` or `gh` sign-in copied in acts as that person for everyone who can run the image.

## One replica, one volume

The daemon's state belongs to a single writer. A second daemon on the same volume stops at once with `state directory is in use by another instance`. Run exactly one replica: a Kubernetes StatefulSet or a Deployment with the `Recreate` strategy, an ECS service with a maximum of 100% and a minimum healthy of 0%, or a Nomad group with `count = 1`. A rolling update that starts the new container before the old one stops is refused this way until the old one has gone.

The volume must be writable by uid `10001`. The daemon creates its state and config directories owner-only (`0700`) under `/home/conduit` and refuses a state directory that anyone else can read or that another user owns. It never changes them itself:

- A Docker named volume takes its owner from the image, so it works as it is.
- On Kubernetes, set `fsGroup: 10001` in the pod's `securityContext`.
- A host directory or a volume created as root needs `chown -R 10001:10001` once, or an init container that does it.

The volume must be a block volume, such as EBS, Persistent Disk or Azure Disk. The daemon refuses to start on EFS, Filestore, Azure Files, NFS, SMB or a FUSE bucket mount, because two pods could then write the same database and silently corrupt it.

The control socket is in the state directory, and its path must be at most 100 bytes. Move the state elsewhere with `OPEN_CONDUIT_DATA_DIR`, and keep it on the volume.

[Backups](../backups/) work as on any host, into `backups/` in the state directory by default. Point `--backup-dir` at a second volume, which may be a network mount (doctor warns, so run `backup verify` on its bundles), or copy the bundles off with whatever the cluster already uses. A volume snapshot of a running daemon isn't a consistent backup; a bundle is. To restore one, start the container with `serve --restore BUNDLE` and the bundle mounted: it restores in place before serving, once per bundle, with no shell or separate job ([in a container](../../architecture/recovery/#in-a-container)).

## Stopping and restarting

On `SIGTERM`, the daemon stops serving, stops watching its runs and releases their leases, all within 5 seconds, then exits 0. The default grace period on every orchestrator covers that. The image runs `tini` as PID 1, which passes the signal on and reaps the processes runs leave behind.

Runs started without a sandbox or container are processes in the daemon's own container, so they end when the container stops. When the new daemon starts, it records each run that was in flight as interrupted and applies the binding's retry policy. Claude Code's sessions are in `~/.claude` on the volume, so its next attempt can resume the session rather than start again. After `SIGKILL`, or a node that disappears, the new daemon waits for the old leases to expire first, which takes a minute by default (`--lease-duration`).

## Reach it

- **Webhooks.** Listen on all interfaces, with the URL the cluster's ingress or load balancer gives the listener: `"settings": {"webhooks": {"addr": "0.0.0.0:8787", "public_url": "https://hooks.example.com"}}`. Without webhooks, the daemon reconciles with each service on an interval and nothing is lost.
- **The CLI.** Use `kubectl exec` (or `docker exec`, `aws ecs execute-command`) for the owner's commands. Use the [management API over TLS](../team/#reach-the-daemon-over-tls) (`--management-addr 0.0.0.0:8443` with a certificate from a mounted Secret) for people with [their own credentials](../team/#3-a-credential-and-a-role-for-each-person).
- **The web view** serves loopback only. Put a sign-in proxy in the same pod, in front of `--web-addr 127.0.0.1:7420`, as [the team guide](../team/#4-the-web-view-behind-your-own-sign-in) describes for a server.
- **Probes.** `--probe-addr 0.0.0.0:8080` serves `/livez` and `/readyz` with no credential and nothing but a one-word state ([probes](/open-conduit/architecture/config-management/#probes)). `/livez` fails once the host loop has gone 2 minutes without completing a pass. `/readyz` passes once the daemon is running and its storage answers, and fails again while it stops. A degraded daemon (a package that didn't start, say) stays ready, so a restart doesn't end the runs the rest is serving: `open-conduit doctor` says what is wrong. Point a startup probe at `/readyz` too, because `/livez` passes while the daemon is starting. The daemon also exits on an error it can't recover from, and the orchestrator restarts it.

## Where the runs go

A run without a sandbox or container runs as the daemon's user, in its container. It shares the pod's CPU, memory and network, can read everything the daemon's user can, and has the daemon's environment. Size the pod for the daemon, every package it starts (each runs as a process of its own) and `settings.concurrency` agent turns at once. Keep anything you wouldn't hand to an agent out of the container's environment, and use [`secret_env`](#replace-each-interactive-sign-in) for what the agent does need.

To move runs out of the daemon's container:

- **[Remote runners](../remote-runners/)** run each binding's runs on another machine over SSH. The image has the SSH client. Keep the provider's `identity` key on the volume, owner-only, and its `host_key` in the config.
- **The [Docker container provider](../sandboxes/#containers)** needs a Docker daemon. A pod has none, and mounting the node's Docker socket hands the node to anything in the pod. Use it with plain Docker on a host you control, not on a cluster.
- **The [process sandbox](../sandboxes/#process-sandboxes)** (`srt`) isn't in the image, and it needs unprivileged user namespaces, which most clusters don't give a pod.

No container provider yet starts each run in a pod of its own.

## Claude Code on Bedrock or Vertex AI

Claude Code reads the variables that choose its cloud from the binding's `env`, and their credentials from `secret_env`.

**Bedrock with the `aws` extension.** The daemon holds a base identity, and each run gets 15-minute credentials for a role, through a profile that the [AWS extension](/open-conduit/architecture/aws-extension/) writes into the run. Give the binding a role whose policy allows `bedrock:InvokeModel` and `bedrock:InvokeModelWithResponseStream`, grant it `aws.credentials`, and select that profile:

```json
"config": {"playbook": "work-issue",
           "env": {"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "us-east-1", "AWS_PROFILE": "bedrock"}}
```

where `bedrock` is the role's `name` in the `aws` entry. The base identity comes from the connection's key pair, or from `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` in the daemon's environment. To use a pod or task role instead, set the connection's `profile`, which needs the AWS CLI added to the image.

**Bedrock with a key.** Select the key's secrets and map them in `secret_env`: a Bedrock API key to `AWS_BEARER_TOKEN_BEDROCK`, or a key pair to `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. Set `CLAUDE_CODE_USE_BEDROCK` and `AWS_REGION` in `env` as above. The key stays in the run for as long as it is valid, so prefer the `aws` extension.

**Vertex AI.** Set `{"CLAUDE_CODE_USE_VERTEX": "1", "CLOUD_ML_REGION": "us-east5", "ANTHROPIC_VERTEX_PROJECT_ID": "my-project"}` in `env`. On GKE with Workload Identity, the run gets the pod's service account from the metadata server and needs nothing else. Elsewhere, mount a service account key into the container and set `GOOGLE_APPLICATION_CREDENTIALS` to its path in `env`. Anything the run can reach, its agent can read too.

## Usage telemetry

A release build sends anonymous [usage reports](/open-conduit/reference/telemetry/), and the image is one. Set `OPEN_CONDUIT_NO_TELEMETRY=1` (or `DO_NOT_TRACK=1`) in the container's environment to turn them off. The install ID is kept in the state directory, so it lasts as long as the volume does.

## A Kubernetes example

This runs one daemon with its config from a ConfigMap, its credentials from a Secret, its state on a persistent volume and webhooks behind a Service:

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: open-conduit
spec:
  replicas: 1                      # never more: the state has one writer
  serviceName: open-conduit
  selector:
    matchLabels: {app: open-conduit}
  template:
    metadata:
      labels: {app: open-conduit}
    spec:
      securityContext:
        runAsUser: 10001
        runAsGroup: 10001
        fsGroup: 10001
        fsGroupChangePolicy: OnRootMismatch
      containers:
        - name: open-conduit
          image: ghcr.io/open-conduit/open-conduit:X.Y.Z
          args: [--probe-addr, "0.0.0.0:8080"]
          ports:
            - {name: webhooks, containerPort: 8787}
            - {name: probes, containerPort: 8080}
          startupProbe:                   # packages start before it is ready
            httpGet: {path: /readyz, port: probes}
            periodSeconds: 5
            failureThreshold: 60
          readinessProbe:
            httpGet: {path: /readyz, port: probes}
          livenessProbe:
            httpGet: {path: /livez, port: probes}
            periodSeconds: 30
          envFrom:
            - secretRef: {name: open-conduit}    # CLAUDE_TOKEN, GITHUB_APP_KEY, …
          resources:
            requests: {cpu: "1", memory: 2Gi}   # a starting point: see where the runs go
          volumeMounts:
            - {name: home, mountPath: /home/conduit}
            - {name: config, mountPath: /etc/open-conduit, readOnly: true}
      volumes:
        - name: config
          configMap: {name: open-conduit}       # holds config.json
  volumeClaimTemplates:
    - metadata: {name: home}
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests: {storage: 20Gi}
---
apiVersion: v1
kind: Service
metadata:
  name: open-conduit
spec:
  selector: {app: open-conduit}
  ports:
    - {name: webhooks, port: 8787, targetPort: webhooks}
```

```sh
kubectl create configmap open-conduit --from-file=config.json
kubectl create secret generic open-conduit \
  --from-literal=CLAUDE_TOKEN="$(cat claude-token)" --from-file=GITHUB_APP_KEY=acme-agents.private-key.pem
kubectl apply -f open-conduit.yaml
kubectl exec open-conduit-0 -- open-conduit doctor
```

The volume holds every workspace, so size it for the repositories you work on and the number of Work items open at once. `ReadWriteOnce` is enough, since only one pod ever mounts it.
