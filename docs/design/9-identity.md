# 9. Identity

This section covers who the two workloads are in Azure and how they prove it: the analyzer (9.1), the executor and its GitHub App key (9.2), the workload identity wiring (9.3), how Key Vault signing fails and retries (9.4) and the Go side (9.5). Edge cases and numbers are in the table "Details" at the end (9.6).

[![Workload identity chain for the analyzer and the executor: pod and ServiceAccount, projected token, federated credential in Entra ID, managed identity, then Foundry and the analysis records container, or a sign-only Key Vault key.](../assets/diagrams/identity.svg)](../assets/diagrams/identity.svg "Open full size")

The rule for everything below: no PATs, no client secrets, no long-lived API keys. Tokens for Foundry, Key Vault and GitHub are never interchangeable.

## 9.1. Analyzer

**What it does.** The analyzer has its own managed identity. It holds only the inference role on the model deployment, and permission to create blobs in the analysis records container (ADR-0015). It asks Entra ID for a token of the Cognitive Services audience for the model and, only with a record store configured, one of the Storage audience for the records (`internal/providers/store/azureblob`, azidentity's workload identity credential).

**Why.** The analyzer talks to a model and writes records.

**Limits.** The token audience must match the endpoint. The deployed role can still overwrite the records, so the adapter sends every write with `If-None-Match: *`: Blob Storage then refuses to replace an existing blob (409 or 412), and the adapter never retries that answer. That protects against the analyzer's own mistakes, not against a stolen token, which can still send a write without the header. Versioning is the recovery evidence until [#154](https://github.com/pkalin/shadewright/issues/154) makes the role create-only (planned).

## 9.2. Executor and the GitHub App key

**What it does.** The executor has its own managed identity. It may only *sign* with one Key Vault key. The GitHub App private key is imported into Key Vault as a non-exportable key. The executor signs the App JWT through Key Vault and exchanges it for a one-hour installation token.

**Why.** The private key never exists in the pod or in a Kubernetes Secret ([ADR-0006](../adr/0006-github-app-key-in-key-vault.md)). A compromised pod cannot copy the key out, but while it runs it can still sign with it ([threat model](../threat-model.md)).

**The JWT and the token.**

- The JWT is issued 60 seconds in the past and lives 10 minutes in total.
- The token request names the one repository and the four permissions of section 8.4.
- The response is refused unless it lists exactly that repository.
- The token is cached until 5 minutes before it expires, and concurrent callers share one refresh.
- The token prints as a placeholder, never as its value (`internal/providers/github/auth`).

## 9.3. Workload identity wiring

**What it does.** Federated credentials use subject `system:serviceaccount:<namespace>:<serviceaccount>` and audience `api://AzureADTokenExchange` ([AKS workload identity](https://learn.microsoft.com/en-us/azure/aks/workload-identity-overview)). With `config/` the executor's subject is `system:serviceaccount:shadewright-system:shadewright-executor` (the lab default since [shadewright-azure#7](https://github.com/pkalin/shadewright-azure/pull/7)).

**What the pod needs.**

- The executor pod carries the label `azure.workload.identity/use: "true"`.
- Its ServiceAccount carries the annotation `azure.workload.identity/client-id` with the executor identity's client ID. In `config/` this is a placeholder that the deployer sets.

**What can go wrong: missing label or annotation.** Without them the executor cannot sign, and every repository open fails as a transient error. It is retried with backoff, so requests wait in their phase and the log shows the failure.

**What can go wrong: unusable repository settings.** The repository settings are validated before the signer is built. An entry the adapter refuses is reported as `RepositoryUnusable` and is not retried.

## 9.4. Signing through Key Vault: deadlines and retries

**What it does.** A sign call through Key Vault (`internal/providers/signer/azurekv`) has a 30-second deadline, or the caller's if that is shorter. Each try has a 10-second deadline. Within the deadline, failures that look temporary are retried up to 3 times with exponential backoff and jitter.

**Retried (transient):**

- throttling and transient server errors;
- network failures: TLS alerts from the server, a closed idle connection, HTTP/2 GOAWAY or stream resets, and any HTTP client error not known to be local;
- a response body that breaks off.

**Reported as transient**, so the caller retries later:

- what is still failing after the 3 retries;
- a token request that gets no answer from Entra ID;
- a token request that gets a successful answer that cannot be used;
- a token request that runs into the per-try deadline;
- a response from Key Vault or Entra ID that the HTTP client hands back only after the try's deadline has passed, when it has no body (an empty 200, for example) or its body was still being read (#447).

**Final (reported as permanent; the caller does not retry):**

- a certificate the client refuses, from Key Vault or Entra ID;
- an https request answered in plain HTTP;
- a token request Entra ID refuses;
- a federated token file that cannot be read (see 9.6);
- a token failure the signer cannot see into, from a credential it did not build;
- a caller that cancels.

**Limits.** When Entra ID's answer reaches the signer before the try's deadline, its status decides, even if the deadline passes before the token request returns, unless the caller cancelled. Errors keep only fixed text, the status code and the class.

## 9.5. In Go

**What it does.** Workload identity goes through `azidentity` (or a deliberately configured `DefaultAzureCredential`).

**How the signer does it.** The Key Vault signer builds `azidentity.ClientAssertionCredential` with the same tenant, client ID and authority host as `WorkloadIdentityCredential`. It reads the federated token file itself and sends its token requests through its own HTTP transport. That way it knows when the file, or the connection to Entra ID, is the failure.

## 9.6. Details

| Topic | Detail |
| --- | --- |
| Installation token | One hour; checked to list only the one repository; requests the four permissions of section 8.4 |
| JWT | Issued 60 seconds in the past, 10 minutes in total |
| Token cache | Until 5 minutes before expiry; one shared refresh |
| Sign call deadline | 30 seconds, or the caller's if shorter; 10 seconds per try |
| Sign retries | Up to 3, exponential backoff with jitter |
| Federated token file, final failures | Missing also after one retry 50 ms later, not a regular file, empty, or larger than 64 KiB |
| Analysis records role | Can overwrite until [#154](https://github.com/pkalin/shadewright/issues/154); versioning is the recovery evidence |
