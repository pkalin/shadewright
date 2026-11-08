# Changelog

## [0.3.1](https://github.com/pkalin/shadewright/compare/v0.3.0...v0.3.1) (2026-11-08)


### Bug fixes

* **gitops:** open or adopt a pull request only when its branch holds nothing but the planned change ([#560](https://github.com/pkalin/shadewright/issues/560)) ([df2e203](https://github.com/pkalin/shadewright/commit/df2e203bca7df94c0a8182597b33cab06c8a5e4e)), closes [#524](https://github.com/pkalin/shadewright/issues/524)

## [0.3.0](https://github.com/pkalin/shadewright/compare/v0.2.0...v0.3.0) (2026-11-08)


### ⚠ BREAKING CHANGES

* **api:** the RemediationRequest CRD declares status.phase, status.holdsTarget and status.breakerProbe as selectable fields and the executor lists with them, so Kubernetes 1.32 or later is required (chart kubeVersion >=1.32.0-0).
* **chart:** an install that removes or empties executor.resources.limits.memory or analyzer.resources.limits.memory is refused by the schema. Set a memory limit instead (the default is 128Mi).

### Features

* **analyzer:** add the evaluation harness for the lab scenarios ([#461](https://github.com/pkalin/shadewright/issues/461)) ([f317f3f](https://github.com/pkalin/shadewright/commit/f317f3f871f849bc9470d27a3166cca1ba798713))
* **analyzer:** add the read-only tools for the model ([#414](https://github.com/pkalin/shadewright/issues/414)) ([9853b59](https://github.com/pkalin/shadewright/commit/9853b592ed26e363a6475ff7a34c6cecdc2e6740))
* **analyzer:** build the bounded, redacted incident context ([#395](https://github.com/pkalin/shadewright/issues/395)) ([8d097ae](https://github.com/pkalin/shadewright/commit/8d097aea46f68327421ed585c789e2943cb24d8e))
* **analyzer:** consume the KEDA HPA symptom source as Incident inputs ([#408](https://github.com/pkalin/shadewright/issues/408)) ([08488ce](https://github.com/pkalin/shadewright/commit/08488ce17bb420c55b875a670671ed0ff6397cee))
* **analyzer:** infer a missing Secret or ConfigMap from pod status and Events ([#381](https://github.com/pkalin/shadewright/issues/381)) ([71d5d17](https://github.com/pkalin/shadewright/commit/71d5d1798a2169c85117dc2ed09d29643009e85f))
* **analyzer:** log the missing data a hand-off names ([#426](https://github.com/pkalin/shadewright/issues/426)) ([0827e53](https://github.com/pkalin/shadewright/commit/0827e53e9c223bfbc0c8ef5622d46851c9935cdf))
* **analyzer:** open a new Incident when a resolved one recurs ([#468](https://github.com/pkalin/shadewright/issues/468)) ([bc59326](https://github.com/pkalin/shadewright/commit/bc5932698d3c1d1c07a908cb5ba87dd2a03af26b))
* **analyzer:** open Incidents from the Flux, Kyverno, cloudflared and KEDA symptom sources ([#478](https://github.com/pkalin/shadewright/issues/478)) ([1fc74a8](https://github.com/pkalin/shadewright/commit/1fc74a821b8eb38aa613fb9ddee962f1ac8debc8))
* **analyzer:** open Incidents only for symptoms that recur or are immediate ([#417](https://github.com/pkalin/shadewright/issues/417)) ([017178a](https://github.com/pkalin/shadewright/commit/017178a66dfea4ed49c61dacd097da0882d44bc2))
* **analyzer:** resolve and escalate Incidents from quiet time and request phases ([#477](https://github.com/pkalin/shadewright/issues/477)) ([b333017](https://github.com/pkalin/shadewright/commit/b33301732e277695333e589befc9556607f59009))
* **analyzer:** run the bounded analysis loop with the model ([#415](https://github.com/pkalin/shadewright/issues/415)) ([f7d6dbd](https://github.com/pkalin/shadewright/commit/f7d6dbd2c69645835ad1a038a1d7fd9983f0bbac))
* **analyzer:** show the model the policy bounds that decide its proposal ([#500](https://github.com/pkalin/shadewright/issues/500)) ([ef1b2f9](https://github.com/pkalin/shadewright/commit/ef1b2f9aa4803611a0a45433296a13024d75c0cc))
* **analyzer:** skip a first analysis of an Incident not seen recently ([#428](https://github.com/pkalin/shadewright/issues/428)) ([e7ee7fa](https://github.com/pkalin/shadewright/commit/e7ee7fa75fa6a776224895d0a5fb6e429d321c89))
* **analyzer:** start the analysis loop with the Foundry adapter behind --triage-model ([#421](https://github.com/pkalin/shadewright/issues/421)) ([90c7eaf](https://github.com/pkalin/shadewright/commit/90c7eaf37f1c4b1a8cb49c9194b28509d0b58998))
* **analyzer:** trace each analysis with OpenTelemetry ([#467](https://github.com/pkalin/shadewright/issues/467)) ([b8dfed3](https://github.com/pkalin/shadewright/commit/b8dfed3b9f4febbbb02f68d20c4ee6b7b033f618))
* **analyzer:** wire the Prometheus and pod log sources into the incident context ([#406](https://github.com/pkalin/shadewright/issues/406)) ([1f3a94c](https://github.com/pkalin/shadewright/commit/1f3a94c75cc88ee4790a49e75538513bccb14e70))
* **analyzer:** write analysis records to Azure Blob Storage ([#451](https://github.com/pkalin/shadewright/issues/451)) ([7c3d6a4](https://github.com/pkalin/shadewright/commit/7c3d6a4a34e4f7c22d28b064733549ab150869e9))
* **api:** add a read-only kubectl shadewright status plugin and printer columns ([#441](https://github.com/pkalin/shadewright/issues/441)) ([034ff3a](https://github.com/pkalin/shadewright/commit/034ff3a096d7ad1019e85fe9c62cc764c0cbf1b7))
* **api:** list the requests for the executor limits with field selectors ([#377](https://github.com/pkalin/shadewright/issues/377)) ([406818a](https://github.com/pkalin/shadewright/commit/406818ae64e6fca381e07b3d46855d4063549940))
* **chart:** add an optional TLS trust profile for the metrics endpoints ([#359](https://github.com/pkalin/shadewright/issues/359)) ([c767ab8](https://github.com/pkalin/shadewright/commit/c767ab81bd500ae9cad7fa8c7d1aa3fd5ffaabe0))
* **chart:** one DNS egress profile for the workloads and the Helm test ([#355](https://github.com/pkalin/shadewright/issues/355)) ([fd6d141](https://github.com/pkalin/shadewright/commit/fd6d14182502c4ba89cdb4c7eff73779f1ba4c91)), closes [#340](https://github.com/pkalin/shadewright/issues/340)
* **chart:** optional analyzer-only Prometheus egress rule and network policy docs ([#383](https://github.com/pkalin/shadewright/issues/383)) ([44c22e1](https://github.com/pkalin/shadewright/commit/44c22e1dd722c8ab1694fc3f184f714af4890728))
* **chart:** set the analyzer's context sources from values and grant pods/log only when used ([#416](https://github.com/pkalin/shadewright/issues/416)) ([f818d34](https://github.com/pkalin/shadewright/commit/f818d34b35a081270d664ccc1f4c36beb02b3752))
* **docs:** open diagrams in a zoomable overlay ([#405](https://github.com/pkalin/shadewright/issues/405)) ([cb29c46](https://github.com/pkalin/shadewright/commit/cb29c4654038bf4ae2c14e84b3e042eba16e974a))
* **gitops:** list directories so source mapping works without a kustomization file ([#463](https://github.com/pkalin/shadewright/issues/463)) ([15cb4ad](https://github.com/pkalin/shadewright/commit/15cb4ad4eed4f8f6e958e1f9ebfc6e0ad46457f9)), closes [#122](https://github.com/pkalin/shadewright/issues/122)
* **integrations:** add the Microsoft Foundry model provider ([#412](https://github.com/pkalin/shadewright/issues/412)) ([215e6b9](https://github.com/pkalin/shadewright/commit/215e6b9ef89114b48059809269c478c9ad31357d))
* **integrations:** mark a Prometheus answer that carries warnings as partial ([#347](https://github.com/pkalin/shadewright/issues/347)) ([dfb3f63](https://github.com/pkalin/shadewright/commit/dfb3f632bebd741c77722c5671c0b7a080ed73c8))
* **integrations:** select metrics of the target's own pods only ([#351](https://github.com/pkalin/shadewright/issues/351)) ([86a7edf](https://github.com/pkalin/shadewright/commit/86a7edf723da3d1f07de83db882a80150b77d093))
* **integrations:** state the Prometheus source profile and count each measurement once ([#367](https://github.com/pkalin/shadewright/issues/367)) ([a33f229](https://github.com/pkalin/shadewright/commit/a33f22984f1892d8eff1ca267a140eaaddea9361))
* **integrations:** tie each pod log excerpt to the instance it was read from ([#356](https://github.com/pkalin/shadewright/issues/356)) ([cd2e5d3](https://github.com/pkalin/shadewright/commit/cd2e5d3814513607c3f30bbc8c15fbb9c471514e))
* **policy:** bound the circuit breaker's reads with a watermark on the requests ([#464](https://github.com/pkalin/shadewright/issues/464)) ([c6aacc2](https://github.com/pkalin/shadewright/commit/c6aacc2fe7a254cd8206d7fd8522600e8237894b))
* **policy:** refuse requests on their own fields before reading Git ([#410](https://github.com/pkalin/shadewright/issues/410)) ([e97ab43](https://github.com/pkalin/shadewright/commit/e97ab4345dd0dcc535cb1c89e7c93fa0ad057738))
* publish signed kubectl-shadewright binaries with each release ([#443](https://github.com/pkalin/shadewright/issues/443)) ([67ca4df](https://github.com/pkalin/shadewright/commit/67ca4df299c61a78c6e945d7493bda5b3a12ec6b))


### Bug fixes

* **analyzer:** ask for quotes copied from one value and classify refused quotes ([#487](https://github.com/pkalin/shadewright/issues/487)) ([50d2e52](https://github.com/pkalin/shadewright/commit/50d2e52bb568c18a530fa0d4fad92ee02c4e645c))
* **analyzer:** ask the summary to name the subject and the observed values ([#519](https://github.com/pkalin/shadewright/issues/519)) ([0642d53](https://github.com/pkalin/shadewright/commit/0642d53d0e808e906bf32382d476c61fec69c92a))
* **analyzer:** classify a reply racing the model deadline as a timeout ([#442](https://github.com/pkalin/shadewright/issues/442)) ([4539e6d](https://github.com/pkalin/shadewright/commit/4539e6d5430c47e9d9caac9b4c3f7f6c032df7dd))
* **analyzer:** find a quiet spell that ends between two passes and claim only current-generation marks ([#448](https://github.com/pkalin/shadewright/issues/448)) ([561bce7](https://github.com/pkalin/shadewright/commit/561bce7ca49a743079c13b51846cd5934d8c0412))
* **analyzer:** keep milli quantities in shown symptoms and evidence ([#491](https://github.com/pkalin/shadewright/issues/491)) ([db863ec](https://github.com/pkalin/shadewright/commit/db863ecc002feb5ef4daa1cbaf5f9cfdba345501))
* **analyzer:** let a request that ended after a quiet resolve decide the Incident ([#485](https://github.com/pkalin/shadewright/issues/485)) ([feb32cf](https://github.com/pkalin/shadewright/commit/feb32cf40160fc2a95f5ddbfe3108de5671a9924))
* **analyzer:** redact a quoted value continued past its block, and withhold a value when redaction panics ([#529](https://github.com/pkalin/shadewright/issues/529)) ([f0a5bda](https://github.com/pkalin/shadewright/commit/f0a5bda4e6b85a62e45c8c0fc71461dde987416a))
* **analyzer:** start the settle limit after a quiet spell from the analyzer's own sight ([#445](https://github.com/pkalin/shadewright/issues/445)) ([5096771](https://github.com/pkalin/shadewright/commit/5096771279c8ae88af634bf1b79dad0cafe72a43))
* **analyzer:** stop AnalysisDue saying Analyzing after the run ends ([#435](https://github.com/pkalin/shadewright/issues/435)) ([f3c91ee](https://github.com/pkalin/shadewright/commit/f3c91ee857132c7411163c1d3c17abd3451e8864))
* **analyzer:** take the previous memory limit from the pod template ([#498](https://github.com/pkalin/shadewright/issues/498)) ([e5938cb](https://github.com/pkalin/shadewright/commit/e5938cbfda7f850d7e88e4f006c5f5f3252c9421))
* **analyzer:** tidy the analysis record follow-ups of the second [#451](https://github.com/pkalin/shadewright/issues/451) review ([#457](https://github.com/pkalin/shadewright/issues/457)) ([1eaa826](https://github.com/pkalin/shadewright/commit/1eaa826f04881450e27ab03f68e9d1a7f69279d0))
* **chart:** keep the analysis budget through a Helm uninstall ([#349](https://github.com/pkalin/shadewright/issues/349)) ([05f6c2b](https://github.com/pkalin/shadewright/commit/05f6c2b38838b67ddd71a93585fe1c6bcd2154b1)), closes [#335](https://github.com/pkalin/shadewright/issues/335)
* **chart:** require a memory limit for both components in the values schema ([#346](https://github.com/pkalin/shadewright/issues/346)) ([8cb952b](https://github.com/pkalin/shadewright/commit/8cb952b489c750e485927dcd752e568870909e99))
* **ci:** publish a release only once its assets are attached ([#455](https://github.com/pkalin/shadewright/issues/455)) ([6bac177](https://github.com/pkalin/shadewright/commit/6bac177648cd7b8154a111d815cf2413fe173123))
* **deps:** bump github.com/google/cel-go to v0.30.0 for GO-2026-6094 ([#358](https://github.com/pkalin/shadewright/issues/358)) ([afca7d9](https://github.com/pkalin/shadewright/commit/afca7d9b65da1ae06c55cccc6635b7959f80a9f0))
* **gitops:** bound source mapping downloads by the read budget ([#460](https://github.com/pkalin/shadewright/issues/460)) ([838053f](https://github.com/pkalin/shadewright/commit/838053f7217a0d9addd82725f9880e729b4a6591)), closes [#298](https://github.com/pkalin/shadewright/issues/298)
* **gitops:** bound the build before the Kustomize render ([#496](https://github.com/pkalin/shadewright/issues/496)) ([8d8478d](https://github.com/pkalin/shadewright/commit/8d8478d193f9d10dfbbe9fe3fa7b2ad3037acff2))
* **gitops:** check the kill switch before every write request and retry ([#357](https://github.com/pkalin/shadewright/issues/357)) ([824073e](https://github.com/pkalin/shadewright/commit/824073ea6d6489b4fe5a25b5044a84c9211d9b43))
* **gitops:** classify an empty Key Vault answer after the try deadline as a timeout ([#450](https://github.com/pkalin/shadewright/issues/450)) ([abd09e5](https://github.com/pkalin/shadewright/commit/abd09e552b5980ec087794e9a9055e23a971851a))
* **gitops:** fit the Kustomize build budget inside the executor's memory limit ([#490](https://github.com/pkalin/shadewright/issues/490)) ([78b6640](https://github.com/pkalin/shadewright/commit/78b66403fdf3116680f5d12db434ab678456936b))
* **gitops:** keep the request name in the pull request body ([#436](https://github.com/pkalin/shadewright/issues/436)) ([134210c](https://github.com/pkalin/shadewright/commit/134210c658770f3121c0a6f4beb4f54a3251b449))
* **gitops:** look at a waiting pull request every minute ([#444](https://github.com/pkalin/shadewright/issues/444)) ([6eecc37](https://github.com/pkalin/shadewright/commit/6eecc37b65782ea2ae0f307f2993e8e03b102bff))
* give each Key Vault request attempt its own copy of the body ([#348](https://github.com/pkalin/shadewright/issues/348)) ([c8f3df4](https://github.com/pkalin/shadewright/commit/c8f3df45fc2be28da96fe21d5e9a84890e447f37))
* **policy:** hold the target lock until the pull request is known closed ([#375](https://github.com/pkalin/shadewright/issues/375)) ([f1e6e4d](https://github.com/pkalin/shadewright/commit/f1e6e4dd0ce65695efb54274627e82f275566345))
* **release:** publish a release only once its assets are attached ([6bac177](https://github.com/pkalin/shadewright/commit/6bac177648cd7b8154a111d815cf2413fe173123))

## [0.2.0](https://github.com/pkalin/shadewright/compare/v0.1.0...v0.2.0) (2026-11-04)


### ⚠ BREAKING CHANGES

* **api:** RemediationSettings spec.maxAnalysesPerDay moved to spec.analysis.maxAnalysesPerDay. The old field stays as deprecated: any write that sets or changes it is refused, and while a stored value remains the analyzer uses the lower of the two. When upgrading from v0.1.0, move the value in one patch, for example kubectl patch remediationsettings default --type merge -p '{"spec":{"maxAnalysesPerDay":null,"analysis":{"maxAnalysesPerDay":0}}}', and change the manifests you keep in Git (upgrade and uninstall guide).

### Features

* add a Helm chart published to GHCR on release tags ([#303](https://github.com/pkalin/shadewright/issues/303)) ([69009f3](https://github.com/pkalin/shadewright/commit/69009f3e027c91e97418ba39999c16f51fc12a21))
* add Prometheus metrics for incidents, analyses, requests by phase, denials and errors ([#292](https://github.com/pkalin/shadewright/issues/292)) ([02af160](https://github.com/pkalin/shadewright/commit/02af16045ad79df0220dccf510447ebbefb015a4))
* **analyzer:** add the pod logs LogSource adapter, bounded, scoped and redacted ([#316](https://github.com/pkalin/shadewright/issues/316)) ([b94dea3](https://github.com/pkalin/shadewright/commit/b94dea331f7db5b54c7017530d345f701b22aa56))
* **analyzer:** add the Prometheus MetricsSource adapter with approved query templates ([#304](https://github.com/pkalin/shadewright/issues/304)) ([082137e](https://github.com/pkalin/shadewright/commit/082137e9c74c982c278a9ea162cd270c005ea07f))
* **api:** analysis limits and model configuration in RemediationSettings ([#315](https://github.com/pkalin/shadewright/issues/315)) ([3541011](https://github.com/pkalin/shadewright/commit/3541011598a587920851f2567f8cbfec3ddac8be))
* **chart:** install with default values and meet chart-testing conventions ([#310](https://github.com/pkalin/shadewright/issues/310)) ([ffbd18f](https://github.com/pkalin/shadewright/commit/ffbd18f51dd08138aac40c62f358301c90e675b2))
* **platforms:** detect EKS and GKE with detection-only modules ([#299](https://github.com/pkalin/shadewright/issues/299)) ([1121976](https://github.com/pkalin/shadewright/commit/11219768997373915e3178984e3984983b44e19f))
* **policy:** add a half-open state to the circuit breaker ([#305](https://github.com/pkalin/shadewright/issues/305)) ([109c3c0](https://github.com/pkalin/shadewright/commit/109c3c066549e159c7c80701fe920bf2c416558e))
* **policy:** check the kill switch right before every Git write ([#273](https://github.com/pkalin/shadewright/issues/273)) ([c5bbbc7](https://github.com/pkalin/shadewright/commit/c5bbbc7d0c2820daa5c399a59302fd894e4aa734))
* **policy:** enforce concurrency limits, a lock per target and a circuit breaker ([#291](https://github.com/pkalin/shadewright/issues/291)) ([f6afd4d](https://github.com/pkalin/shadewright/commit/f6afd4d5666c632c8c164a0df6c5c4327e8c1a71))


### Bug fixes

* **analyzer:** bound the occurrences held unwritten per Incident and in total ([#275](https://github.com/pkalin/shadewright/issues/275)) ([1263276](https://github.com/pkalin/shadewright/commit/126327602e481d00255d3a888ff5cd77c2f3a472))
* **analyzer:** give each namespace a share of the occurrences held unwritten ([#301](https://github.com/pkalin/shadewright/issues/301)) ([6ad208c](https://github.com/pkalin/shadewright/commit/6ad208c04300aaf674472f4b3677f0bd5e76b00c))
* **chart:** keep the chart version out of the generated README ([#326](https://github.com/pkalin/shadewright/issues/326)) ([bda424e](https://github.com/pkalin/shadewright/commit/bda424ea5f3d5b52ac3d5c488b96f568be96de4a))
* **gitops:** give the Incident read in the pull request step its own deadline ([#277](https://github.com/pkalin/shadewright/issues/277)) ([1403ca4](https://github.com/pkalin/shadewright/commit/1403ca475f9427686fdb623c8fa5ec76bb845106))
* **gitops:** name the Incident in the pull request body once the executor has read it back ([#265](https://github.com/pkalin/shadewright/issues/265)) ([1c10e67](https://github.com/pkalin/shadewright/commit/1c10e6731e8066df3bdfeffd87b08f2afec97ce8))
* **gitops:** say on PullRequestOpened when the pull request is closed ([#270](https://github.com/pkalin/shadewright/issues/270)) ([d85178f](https://github.com/pkalin/shadewright/commit/d85178fba8ae92c8841830bf499f96efa37003aa))
* **gitops:** stop source mapping at the second declaration and bound its total input ([#296](https://github.com/pkalin/shadewright/issues/296)) ([d2e3a69](https://github.com/pkalin/shadewright/commit/d2e3a69b0b8a708d2ddbf390fb78f29caf46e9fe))


### Refactoring

* **api:** move maxAnalysesPerDay under spec.analysis ([3dff1db](https://github.com/pkalin/shadewright/commit/3dff1dbb5c10c9da1d58a025280b59b705f53a47))

## 0.1.0 (2026-11-04)


### Features

* add incident fingerprint and symptom normalization ([#111](https://github.com/pkalin/shadewright/issues/111)) ([cfbf840](https://github.com/pkalin/shadewright/commit/cfbf840a6a65002dc64b705e627d79013ac53af5))
* add redaction of secrets and personal data ([#110](https://github.com/pkalin/shadewright/issues/110)) ([d82a45e](https://github.com/pkalin/shadewright/commit/d82a45ee4502b5145d2868f47db248c961938a73))
* **analyzer:** bound the analyzer's reads and Incident writes to the selected namespaces ([#168](https://github.com/pkalin/shadewright/issues/168)) ([3ceba2e](https://github.com/pkalin/shadewright/commit/3ceba2e9d9d580b8633a294aea6bc333ec5d0e87))
* **analyzer:** collect events with series handling and owner correlation ([#185](https://github.com/pkalin/shadewright/issues/185)) ([f124290](https://github.com/pkalin/shadewright/commit/f12429012313745fed028d63a7af46d911d6e1fc))
* **analyzer:** keep one durable Incident per fingerprint ([#196](https://github.com/pkalin/shadewright/issues/196)) ([e49ce5b](https://github.com/pkalin/shadewright/commit/e49ce5ba57d96be590e77237b1ccdf635d0f638d))
* **analyzer:** mark Incidents due for analysis and keep the daily budget in its own ConfigMap ([#201](https://github.com/pkalin/shadewright/issues/201)) ([22d3d3b](https://github.com/pkalin/shadewright/commit/22d3d3be6682329b5d034c14840f7c1cad40a604))
* **api:** enable modules per cluster in RemediationSettings and report detection ([#220](https://github.com/pkalin/shadewright/issues/220)) ([1dce946](https://github.com/pkalin/shadewright/commit/1dce94605f0c4b65559d77680dfa2b46a9252077))
* **gitops:** add Azure Key Vault signer adapter ([#118](https://github.com/pkalin/shadewright/issues/118)) ([1eaea9f](https://github.com/pkalin/shadewright/commit/1eaea9f69af3ee598a1dff89ef88c07674ae80e9))
* **gitops:** add the GitHub adapter of GitProvider ([#139](https://github.com/pkalin/shadewright/issues/139)) ([ea008fe](https://github.com/pkalin/shadewright/commit/ea008fe4871260f4345a3a7bb21db88347893b72))
* **gitops:** add typed patchers for memory limit and KEDA max replicas ([#137](https://github.com/pkalin/shadewright/issues/137)) ([90eb0af](https://github.com/pkalin/shadewright/commit/90eb0af09752f683347f83f7ac4384f2e33fa78f))
* **gitops:** authenticate as a GitHub App with a signer-backed installation token cache ([#131](https://github.com/pkalin/shadewright/issues/131)) ([a823f00](https://github.com/pkalin/shadewright/commit/a823f00b68e466674c3f42c4048421947613dc38))
* **gitops:** map a target object to its source file in Git ([#120](https://github.com/pkalin/shadewright/issues/120)) ([e560124](https://github.com/pkalin/shadewright/commit/e5601247bdb3f8dc0475b7a2cc33684af828d30c))
* **gitops:** open one pull request per allowed request ([#177](https://github.com/pkalin/shadewright/issues/177)) ([d913f1b](https://github.com/pkalin/shadewright/commit/d913f1b3667d31a927e00a7bf8dfb91c928a50d4))
* **gitops:** render and check the semantic diff before committing ([#176](https://github.com/pkalin/shadewright/issues/176)) ([852ea71](https://github.com/pkalin/shadewright/commit/852ea71c95a7525ce0e14cea75363f83c1362f84))
* **gitops:** run the semantic diff check before every commit of the pull request step ([#182](https://github.com/pkalin/shadewright/issues/182)) ([341fc49](https://github.com/pkalin/shadewright/commit/341fc49f9bb6ec6b0b78b9ad02889d0970bb7efb))
* **gitops:** wire the GitHub adapter and Key Vault signer into the executor ([#148](https://github.com/pkalin/shadewright/issues/148)) ([1892535](https://github.com/pkalin/shadewright/commit/1892535760471d25bda4d116f806abf80b5ebdbe))
* **integrations:** add the cloudflared module with a tunnel down symptom from pod status ([#233](https://github.com/pkalin/shadewright/issues/233)) ([a5c02b8](https://github.com/pkalin/shadewright/commit/a5c02b898edaa88b55e18d139ce61533f0d70368)), closes [#67](https://github.com/pkalin/shadewright/issues/67)
* **integrations:** add the Flux module with Kustomization, HelmRelease and GitRepository symptoms ([#224](https://github.com/pkalin/shadewright/issues/224)) ([b1c21df](https://github.com/pkalin/shadewright/commit/b1c21dfd8bd1dd4c8091765e473af0b31d6aebe5))
* **integrations:** add the KEDA module with trigger error, not ready and replica ceiling symptoms ([#227](https://github.com/pkalin/shadewright/issues/227)) ([db06dd5](https://github.com/pkalin/shadewright/commit/db06dd5698a951cdeb18e39f78ebd88f15179dc0))
* **integrations:** add the Kyverno module with policy report, policy readiness and admission refusal diagnosis ([#229](https://github.com/pkalin/shadewright/issues/229)) ([526d640](https://github.com/pkalin/shadewright/commit/526d640d7a5ab3fe11c538df46411e0976656b3c)), closes [#65](https://github.com/pkalin/shadewright/issues/65)
* **integrations:** add the Secrets Store CSI driver module with mount failure diagnosis ([#231](https://github.com/pkalin/shadewright/issues/231)) ([583906a](https://github.com/pkalin/shadewright/commit/583906ac9eb9d9b1fa0834167d82db254ff6c47d)), closes [#66](https://github.com/pkalin/shadewright/issues/66)
* **platforms:** add registries for platform and integration modules ([#214](https://github.com/pkalin/shadewright/issues/214)) ([a96c584](https://github.com/pkalin/shadewright/commit/a96c5847fc72c866d83b937ee4ecbe34d73e746c))
* **platforms:** detect the platform, report it in settings status and add the generic and AKS modules ([#226](https://github.com/pkalin/shadewright/issues/226)) ([0b0670a](https://github.com/pkalin/shadewright/commit/0b0670aad008a491b4e93fd5a3eddf8c7454cee1)), closes [#61](https://github.com/pkalin/shadewright/issues/61)
* **policy:** add canonical plan digest ([#108](https://github.com/pkalin/shadewright/issues/108)) ([df58f70](https://github.com/pkalin/shadewright/commit/df58f7088193acbd79f59a91aeb577d6b44bc867))
* **policy:** add path matcher for GitPathPattern ([#109](https://github.com/pkalin/shadewright/issues/109)) ([4c391d3](https://github.com/pkalin/shadewright/commit/4c391d3632cd19d152b6e007d0312a70d73fc8f6))
* **policy:** add pure policy decision function ([#117](https://github.com/pkalin/shadewright/issues/117)) ([ffb6714](https://github.com/pkalin/shadewright/commit/ffb67145e2f624a5cdbba1435deeaa9c4f3cc6ed))
* **policy:** default-deny NetworkPolicies and per-component egress ([#213](https://github.com/pkalin/shadewright/issues/213)) ([a98fbdd](https://github.com/pkalin/shadewright/commit/a98fbddc421931e1ea3bc420d49a125f8a24abe6))
* **policy:** plan digest in status and freshness re-check before every write ([#138](https://github.com/pkalin/shadewright/issues/138)) ([dace97e](https://github.com/pkalin/shadewright/commit/dace97e96e02b1601234e897aac23cfac1f88e66))
* **policy:** refuse AdjustKedaMaxReplicas unless the ScaledObject is ready ([#230](https://github.com/pkalin/shadewright/issues/230)) ([da9fbe9](https://github.com/pkalin/shadewright/commit/da9fbe9f91eb4eb3d89341ad350dfd1dc4e08734))
* **policy:** run the policy check phase in the executor ([#130](https://github.com/pkalin/shadewright/issues/130)) ([1310b4c](https://github.com/pkalin/shadewright/commit/1310b4c9f0154dcc8e10105ccba271fd14eadc1f))
* provider interfaces and in-memory fakes ([#102](https://github.com/pkalin/shadewright/issues/102)) ([640d936](https://github.com/pkalin/shadewright/commit/640d93641aaa99008917fe0f0e6dc0c347e6435e))
* split analyzer and executor into two binaries and identities ([#112](https://github.com/pkalin/shadewright/issues/112)) ([3735742](https://github.com/pkalin/shadewright/commit/3735742e92bb63e3dc7ef3f6fad3eaf55782aeb7))


### Bug fixes

* **analyzer:** close the redaction gaps from the reviews of [#110](https://github.com/pkalin/shadewright/issues/110) and [#111](https://github.com/pkalin/shadewright/issues/111) ([#189](https://github.com/pkalin/shadewright/issues/189)) ([a965102](https://github.com/pkalin/shadewright/commit/a9651022362a832f675858e028eb326ef05a18e0))
* **analyzer:** close the redaction leaks found after [#189](https://github.com/pkalin/shadewright/issues/189) ([#192](https://github.com/pkalin/shadewright/issues/192)) ([f82a207](https://github.com/pkalin/shadewright/commit/f82a20751207c0c52ffd83a42ba523f1e842887b))
* **analyzer:** give the budget read and the detection writes their own deadlines ([#263](https://github.com/pkalin/shadewright/issues/263)) ([f2979cd](https://github.com/pkalin/shadewright/commit/f2979cd5ab678eb1ce8a4e6053c5b46a20867300))
* **analyzer:** keep prose after prefixed short keys such as pre-auth ([#200](https://github.com/pkalin/shadewright/issues/200)) ([13f4ba4](https://github.com/pkalin/shadewright/commit/13f4ba4a34fc0682c1abc9486aa9235fa53aac59))
* **api:** align RemediationSettings validation with the GitHub adapter's rules ([#211](https://github.com/pkalin/shadewright/issues/211)) ([6aecc33](https://github.com/pkalin/shadewright/commit/6aecc33bc4aa9c82e53810b26c92b77e1bc1ad75))
* **gitops:** confirm a stale plan and mark its cleanup before closing the pull request ([#210](https://github.com/pkalin/shadewright/issues/210)) ([c7aa3a7](https://github.com/pkalin/shadewright/commit/c7aa3a76a1959666fa800fa0480b634335d0cda8))
* **gitops:** expire a request whose freshness check never passes, with failure tests ([#190](https://github.com/pkalin/shadewright/issues/190)) ([0b420ad](https://github.com/pkalin/shadewright/commit/0b420adfcd3929990078f09f47d84c0fb3c72459))
* **gitops:** handle secondary rate limits and bound the token call by its timeout ([#143](https://github.com/pkalin/shadewright/issues/143)) ([661044a](https://github.com/pkalin/shadewright/commit/661044ad4b525bf22356f16e06a1bd7f32e58142))
* **gitops:** keep raw SDK errors out of the Key Vault signer's error chain ([#183](https://github.com/pkalin/shadewright/issues/183)) ([86c860f](https://github.com/pkalin/shadewright/commit/86c860f2c81bdcf1dbf3ac6b6b88cc08d11d6e23))
* **gitops:** refuse explicit tags on identity fields ([#140](https://github.com/pkalin/shadewright/issues/140)) ([6f93363](https://github.com/pkalin/shadewright/commit/6f93363b3a4bde376ff8f21d8823f9ac2977d032))
* **gitops:** render the change again when the target branch moves ([#198](https://github.com/pkalin/shadewright/issues/198)) ([1f4833c](https://github.com/pkalin/shadewright/commit/1f4833cb7f0609ae74746ab6f1c49c4158cce32e))
* **gitops:** retry transient Key Vault signer failures ([#184](https://github.com/pkalin/shadewright/issues/184)) ([a275fa0](https://github.com/pkalin/shadewright/commit/a275fa0807fce18c4a62ebd32584921733b0ab84))
* **gitops:** stop on GitHub 403 and 422 refusals instead of retrying until the deadline ([#197](https://github.com/pkalin/shadewright/issues/197)) ([a342975](https://github.com/pkalin/shadewright/commit/a34297523bf6b7c517dcf5e7af94da125c98eeda)), closes [#191](https://github.com/pkalin/shadewright/issues/191)
* **integrations:** deep-copy hooks in Selection.Active ([#261](https://github.com/pkalin/shadewright/issues/261)) ([c67cfbe](https://github.com/pkalin/shadewright/commit/c67cfbecdc0373ef2aaa6ad9a716c12ee0f57d6c))
* **integrations:** keep a per-try timeout retryable when the cut body reads cleanly ([#206](https://github.com/pkalin/shadewright/issues/206)) ([0c553c8](https://github.com/pkalin/shadewright/commit/0c553c89a6da5fbc054b7ab0d3bbf12d001073f8))
* **policy:** bound check and cleanup with deadlines, re-resolve source before writes ([#142](https://github.com/pkalin/shadewright/issues/142)) ([f75e587](https://github.com/pkalin/shadewright/commit/f75e587260db73bb3c8130399f7e5eefee4f1b4b))
* **policy:** refuse a fresh verdict that arrives after the deadline ([#150](https://github.com/pkalin/shadewright/issues/150)) ([105a278](https://github.com/pkalin/shadewright/commit/105a278547096dbec820d13dfd0fa55548dbe499))
* **policy:** start the KEDA wait clock when its reason is first set ([#234](https://github.com/pkalin/shadewright/issues/234)) ([459ee86](https://github.com/pkalin/shadewright/commit/459ee86f1f9ad6913da87324765f32b7a3084104))
* start releases at 0.1.0 with a conventional release title ([#113](https://github.com/pkalin/shadewright/issues/113)) ([adc5e05](https://github.com/pkalin/shadewright/commit/adc5e0581d06ef881b93104bf5e7873c833cbc8b))


### Performance

* **gitops:** reuse file reads at a commit across source mappings ([#217](https://github.com/pkalin/shadewright/issues/217)) ([32e8081](https://github.com/pkalin/shadewright/commit/32e80817389fc393b75a9421b5660acf8009abc5))
