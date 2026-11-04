# Changelog

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
