---
title: "AWS extension"
description: "How a run gets short-lived AWS credentials for a named IAM role from the host: the base identity, the account allow-list, the run's profiles, gating a role, and the record of each issue."
---

The [`aws` extension](https://github.com/open-conduit/open-conduit/tree/main/extensions/aws) gives a run short-lived credentials for a named IAM role, from the host, when the run asks. AWS uses the same model for an ECS task or a Lambda function. The daemon holds one base identity for each connection. An entry names the roles that a scope's bindings may assume, and each role must be in an account the entry allows. The extension is first-party and runs out of process as a package bundled with the release (`open-conduit extension-run aws`). It is written only on `protocol/sdk`, as a third party would write it. It uses only public hooks: a connection type, an entry type, one toolbelt operation, a run layer with files ([extension protocol](../extension-protocol/)), a secret reference scheme, a task and two doctor checks.

A key in a binding's `secret_env` stays in the run for the life of the key. Nothing records which run used it, and nobody can approve a write before it happens. This extension issues credentials that last 15 minutes by default. Each role is for named bindings only. CloudTrail names the run behind every call, and a person can gate any role.

## Configuration

The connection holds the base identity and the region:

```json
{"id": "aws-main", "type": "aws", "config": {"region": "eu-west-2"},
 "secret_refs": {"access_key_id": "keyring:aws-base-key-id", "secret_access_key": "keyring:aws-base-secret"}}
```

| Field | Meaning |
| --- | --- |
| `region` | Required. STS is called in this region, and every profile the run gets uses it too. |
| `endpoint` | Optional. A URL the host calls instead of STS's regional endpoint, such as an STS VPC endpoint, LocalStack or moto. The run never calls STS, so this goes nowhere near the run's profiles. |
| `run_endpoint` | Optional. A URL the run's profiles send every AWS call to (`endpoint_url`), for an emulator such as LocalStack that serves every service at one address, so a test deployment talks only to the emulator. Leave it unset for real AWS, so each service's own endpoint is used. |
| `profile` | Optional. The AWS CLI profile that gives the daemon's own identity when the connection has no key pair. |
| `access_key_id`, `secret_access_key` (secrets) | The base identity's key pair. Give both or neither. |

With no key pair, the daemon's own AWS identity is the base identity. That is `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` in the daemon's environment (with `AWS_SESSION_TOKEN` if set) when `profile` is unset. Otherwise it is whatever the AWS CLI's credential chain finds for `profile` or the default profile (an SSO or named profile, an instance profile, a container role), read with `aws configure export-credentials --format process`. Temporary credentials from the chain are kept until five minutes before they expire. The base identity needs `sts:AssumeRole` on each role. Each role's trust policy must trust it, and must also allow `sts:TagSession` for a role that sets `tag_session`.

The entry names the accounts and the roles:

```json
{"name": "main", "type": "aws", "connection": "aws-main", "config": {
  "accounts": ["111122223333"],
  "roles": [
    {"name": "read",  "arn": "arn:aws:iam::111122223333:role/agent-read",  "for_bindings": ["helper"]},
    {"name": "write", "arn": "arn:aws:iam::111122223333:role/agent-write", "for_bindings": ["helper"], "duration": "30m", "tag_session": true}
  ]}}
```

| Field | Meaning |
| --- | --- |
| `accounts` | Required. The 12-digit account IDs a role may be in. A role in any other account is refused when the config is validated, and again at each call. |
| `roles[].name` | The name a run asks for, and its profile's name: a lower-case letter, then up to 31 lower-case letters, digits, `_` or `-`. Unique in the scope. |
| `roles[].arn` | The IAM role's ARN. |
| `roles[].for_bindings` | Required. The bindings that may ask for the role. `config check` refuses a name that isn't a binding of the scope (`missing_binding`). |
| `roles[].duration` | How long the credentials last, from `15m` (the default, and the shortest session STS issues) to `12h`. The role's own maximum session duration must be at least this long, or STS refuses. |
| `roles[].tag_session` | Optional. Also sends the scope, binding, Work and execution as session tags (`open-conduit:scope`, `open-conduit:binding`, `open-conduit:work`, `open-conduit:execution`), which IAM policies and CloudTrail can read. The role's trust policy must allow `sts:TagSession`. |
| `roles[].external_id` | Optional. Sent as `ExternalId` when the role's trust policy asks for one. |

An entry takes no secrets of its own. The scope's `authorization.connections` must grant the connection, and each binding that uses a role must be granted `aws.credentials`.

## The operation

`aws.credentials`, with input `{"role": "read"}`, answers in the `credential_process` format that every AWS SDK reads: `Version`, `AccessKeyId`, `SecretAccessKey`, `SessionToken` and `Expiration`. It reads the calling run's Work and binding from durable state, never from the call. It refuses these calls before STS is called:

- `bad_request`: a call with no role.
- `unknown_role`: a role the scope doesn't have.
- `role_not_granted`: a role that doesn't name the run's binding. Both of these refusals name the roles the run may use.
- `account_not_allowed`: a role outside the entry's accounts.
- `request_id_refused`: a call with a request ID. The result is a credential, and is never kept for replay: the toolbelt keeps no result of an operation with a secret output, and the extension refuses the request ID as well.

Otherwise the host calls `sts:AssumeRole` with the base identity, signed with Signature Version 4, and never follows a redirect. The role session name is `oc.<binding>.<Work ID>`, in the characters STS accepts and at most 64 of them. A Work ID too long to fit keeps its end, after eight hex characters of its SHA-256, so two runs never share a name. CloudTrail records each call the credentials make under that name. An answer that names another account than the role's is refused (`aws_refused`). STS's own refusals are `aws_refused` with STS's error code, never its message. A base identity that can't be used is `aws_credential`, and STS out of reach is `aws_unavailable`.

## The run's profiles

The extension's run layer writes two files into the run ([extension protocol](../extension-protocol/), run environment), for a run whose binding may use at least one role:

- `config`, with a profile for each such role:

  ```ini
  [profile read]
  credential_process = os call aws.credentials --output data --data '{"role":"read"}'
  region = eu-west-2
  ```

  and `endpoint_url` when the connection sets `run_endpoint`.
- `credentials`, empty, so that a profile with the same name in the operator's own `~/.aws/credentials` can't stand in for one of the run's.

It sets `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` to these files, and `AWS_SDK_LOAD_CONFIG=1` for the v1 Go SDK. `aws --profile read s3 ls`, or any SDK with `AWS_PROFILE=read`, then works with no step by the agent. The CLI and the SDKs run the `credential_process` when they need credentials, and again once those expire. `os call --output data` prints only the operation's output data ([shell executor](../shell-executor/)). A run without a toolbelt gets no files, because nothing could answer its profiles.

The agent's guidance for the operation says the same, and that it never needs to call the operation itself.

## Gating a role

A person approves a role with a `human` gate on the operation's input ([gating some inputs](../../workflows/human-decisions/#gating-some-inputs)):

```json
{"operations": ["aws.credentials"], "input": {"role": "write"}, "responders": ["operator:local"]}
```

When the gate holds a call, the operation doesn't run and the run's Work is parked on the approval request, as for any gated call. `os call --output data` prints the gate's answer on stderr and exits 3. The AWS CLI shows that answer to the agent as its credential error. The agent ends its turn and runs the command again once it is resumed. Each AWS CLI command is a new process that asks for credentials once. On its own, an approval lets one identical call through, so each command with a gated profile would ask a person again. Give the gate `valid_for` (the role's `duration` is a natural choice) so an approval covers that role for that Work for a while ([approving for a while](../../workflows/human-decisions/#approving-for-a-while)):

```json
{"operations": ["aws.credentials"], "input": {"role": "write"}, "valid_for": "15m", "responders": ["operator:local"]}
```

## Secrets Manager references

The package also serves the `aws-sm` secret reference scheme ([secrets](../config-management/#secrets-and-process-environment)), so a secret slot anywhere in the config can name a secret in AWS Secrets Manager instead of a copy in the keyring or `secrets.json`:

```json
"secret_refs": {
  "github_token": "aws-sm:prod/github#token",
  "slack_bot": "aws-sm:arn:aws:secretsmanager:eu-west-2:111122223333:secret:prod/slack-AbCdEf"
}
```

`aws-sm:<secret>` is the secret's `SecretString`, and `aws-sm:<secret>#<key>` one string field of a `SecretString` that is a JSON object. `<secret>` is the secret's name or its ARN. The host asks the package for the value only when the slot's consumer needs it (`secret.serve`), and holds the answer for at most a minute per consumer and reference ([secrets](../config-management/#secrets-and-process-environment)), so a webhook check that resolves on every delivery costs one `GetSecretValue` a minute, not one per request, and a rotation in Secrets Manager reaches consumers within that minute with no step in Open Conduit. Each read is in CloudTrail as `GetSecretValue`.

The package reads with the daemon's own AWS identity, never a connection's, so the credentials it serves can't depend on a secret it serves: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY in its environment, else whatever the AWS CLI's credential chain finds (a profile, an instance profile, a container role, or web identity for EKS IRSA). The package's environment is the sanitized baseline, so grant it what that identity needs with `settings.extensions.aws.pass_env`:

```json
"settings": {"extensions": {"aws": {"pass_env": ["AWS_REGION", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE"]}}}
```

The region is the ARN's, else `AWS_REGION` or `AWS_DEFAULT_REGION`. `AWS_ENDPOINT_URL_SECRETS_MANAGER` or `AWS_ENDPOINT_URL` replaces the regional endpoint (a VPC endpoint, LocalStack), as for an AWS SDK. A reference the package can't serve fails closed with a safe code, never AWS's message: `not_found`, `denied`, `decryption_failed`, `unavailable` (a secret marked for deletion), `invalid_reference`, `identity_refused`, `no_identity`, `no_region`, `binary_secret`, `not_json` or `key_missing`. `doctor --check-secrets` and `GET /v1/secrets` show each `aws-sm:` reference and whether it resolves now.

## What the extension does not do

- **The credentials are in the run.** The model can read them. This differs from git's transport, where the host signs ([git transport](../toolbelt-management/#git-transport)). A signing proxy for all of AWS isn't practical, because every service has its own endpoint and Signature Version 4 covers the body. The limits are the role's IAM policy, the short life and the audit trail. An agent that runs the profile's command by hand gets the credentials in its own output, and so does the model. The host doesn't keep them: `aws.credentials` declares `SecretAccessKey` and `SessionToken` as its secret output, so the host scrubs them from everything the run publishes, including the transcript, activity, outcome and live output ([secret outputs](../extensions/#secret-outputs)). The access key ID isn't secret and stays, so it still finds the issue's record.
- **It doesn't open the network.** In a container, add the AWS endpoints the run needs (such as `s3.eu-west-2.amazonaws.com`) to the container's `network.allow`. The host calls STS itself, so STS doesn't need to be listed.
- **No fixed operations** such as `aws.s3.get`.

## Record

Each issue leaves a trail with no credential in it. The record must be written before the credentials are handed out: when it can't be, the call is refused (`aws_unavailable`, try again) and the STS session expires unused.

- A record in the extension's KV namespace `aws.issued`: the scope, Work, execution, binding, entry, role and its ARN, account, session name, access key ID (CloudTrail shows it beside the session name), issue time, expiry, and whether the session was tagged. The `aws-prune` task deletes records older than 90 days, once a day.
- A telemetry event `aws_credentials_issued` linked to the Work, with the same fields except the role ARN and the entry.

The toolbelt's own records show the call in the Work's run history: as called, as gated (`toolbelt.gate`), or as refused.

## Doctor and tests

- `aws`: each entry's base identity is valid (`sts:GetCallerIdentity`), naming the identity's ARN.
- `aws-roles` (deep, `doctor --deep`): the base identity can assume each role, for the shortest session STS issues.

`extensions/aws/awstest` is a fake STS and Secrets Manager that checks each request's signature against the key pairs it knows and answers `GetCallerIdentity` and `AssumeRole` in STS's XML and `GetSecretValue` in Secrets Manager's JSON, so the tests and the certification fixture (`open-conduit extension-run aws certify-fixture`) need no AWS account. The signer is checked against AWS's published Signature Version 4 example.
