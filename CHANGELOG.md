# Changelog

## [0.6.0](https://github.com/pkalin/shadewright/compare/v0.5.2...v0.6.0) (2026-11-21)


### ⚠ BREAKING CHANGES

* **analyzer:** bound the context by maxContextBytes ([#1056](https://github.com/pkalin/shadewright/issues/1056))
* **gitops:** status.mergeContentUnreadSince of RemediationRequest is replaced by status.gitWaits. A content wait running at the upgrade starts again with the next failed read.

### Features

* **analyzer:** add a provider for OpenAI-compatible model endpoints ([#839](https://github.com/pkalin/shadewright/issues/839)) ([5270da5](https://github.com/pkalin/shadewright/commit/5270da51b32b23e939dbd04e3cde5a8a66c3ff71))
* **analyzer:** call Foundry deployments through the Responses API ([#819](https://github.com/pkalin/shadewright/issues/819)) ([d521c23](https://github.com/pkalin/shadewright/commit/d521c232638ae2e7981a1f2828380d5396c1070c)), closes [#501](https://github.com/pkalin/shadewright/issues/501)
* **analyzer:** count a consequence on its cause's Incident ([#807](https://github.com/pkalin/shadewright/issues/807)) ([99b5008](https://github.com/pkalin/shadewright/commit/99b5008aeeda5a9eec37d4c53b4afa87fbc3a941))
* **analyzer:** fold a consequence Incident that opened before its cause ([#811](https://github.com/pkalin/shadewright/issues/811)) ([28a8212](https://github.com/pkalin/shadewright/commit/28a8212cfc8207cc7589c2cc35c09a9d12161646)), closes [#793](https://github.com/pkalin/shadewright/issues/793)
* **analyzer:** hold a consequence with no cause for two minutes ([#802](https://github.com/pkalin/shadewright/issues/802)) ([79e8f7b](https://github.com/pkalin/shadewright/commit/79e8f7bbdbae7af2e7ed37e6b56759326d69a09f))
* **analyzer:** metric templates for the current and target metric of an HPA ([#788](https://github.com/pkalin/shadewright/issues/788)) ([37f54c9](https://github.com/pkalin/shadewright/commit/37f54c9bda502e6a3b98a5d0cfe8440660173a3f))
* **analyzer:** record the container, class and cause of an observation ([#801](https://github.com/pkalin/shadewright/issues/801)) ([471ea1f](https://github.com/pkalin/shadewright/commit/471ea1fa6ba2a31b1e9646cf4e34434d3dcb067b))
* **analyzer:** show the absorbed tally in the model's context ([#829](https://github.com/pkalin/shadewright/issues/829)) ([eea2c05](https://github.com/pkalin/shadewright/commit/eea2c05a67795582e161cc9d7206f8adfedd9981))
* **api:** add the Incident status fields of the cause rule ([#799](https://github.com/pkalin/shadewright/issues/799)) ([61c808a](https://github.com/pkalin/shadewright/commit/61c808a41cea581fbcee5d370e8d2bcc2a2e8289)), closes [#790](https://github.com/pkalin/shadewright/issues/790)
* **api:** cancel a request before the merge with spec.cancel ([#787](https://github.com/pkalin/shadewright/issues/787)) ([38c3596](https://github.com/pkalin/shadewright/commit/38c3596bacf3fe5fb2a97b1b2d85b5904a6358f4)), closes [#289](https://github.com/pkalin/shadewright/issues/289)
* **chart:** add executor.enabled to install the analyzer alone ([#937](https://github.com/pkalin/shadewright/issues/937)) ([c2525c0](https://github.com/pkalin/shadewright/commit/c2525c06229d5bcb34afda070a5def677db72b14))
* **chart:** render the Azure workload identity token without the webhook ([#945](https://github.com/pkalin/shadewright/issues/945)) ([47f1fd6](https://github.com/pkalin/shadewright/commit/47f1fd6b61091c3addcd39cdd3bd1c712475e34c))
* **gitops:** cancel a request whose Incident was folded or is gone ([#816](https://github.com/pkalin/shadewright/issues/816)) ([69dac48](https://github.com/pkalin/shadewright/commit/69dac48d36b1570ee48fbb5ef35ffa60879080de))
* **gitops:** warn when the review check keeps its state without a failure ([#785](https://github.com/pkalin/shadewright/issues/785)) ([a44c9a1](https://github.com/pkalin/shadewright/commit/a44c9a140b7b5a378962857bb1557ade5b738595)), closes [#755](https://github.com/pkalin/shadewright/issues/755)
* **incidents:** count a consequence on its cause's Incident ([#791](https://github.com/pkalin/shadewright/issues/791)) ([99b5008](https://github.com/pkalin/shadewright/commit/99b5008aeeda5a9eec37d4c53b4afa87fbc3a941))
* say when an incident stops while its pull request waits for review ([#800](https://github.com/pkalin/shadewright/issues/800)) ([04dec86](https://github.com/pkalin/shadewright/commit/04dec86567a5751ae201e6a9d014c92188dae027))


### Bug fixes

* **analyzer:** bound the context by maxContextBytes ([#1056](https://github.com/pkalin/shadewright/issues/1056)) ([16406e0](https://github.com/pkalin/shadewright/commit/16406e00ab414fc908143052db511ee40b704a3c)), closes [#981](https://github.com/pkalin/shadewright/issues/981)
* **analyzer:** count held consequences when their cause is consumed ([#822](https://github.com/pkalin/shadewright/issues/822)) ([f102c51](https://github.com/pkalin/shadewright/commit/f102c51ad954b25c5b21f716ecdfa175e8e3f788))
* **analyzer:** do not create a deleted Incident again from a kill counted before the delete ([#905](https://github.com/pkalin/shadewright/issues/905)) ([f56ddba](https://github.com/pkalin/shadewright/commit/f56ddbabc7fa441eedda14af7fe0f41357eed6f6))
* **analyzer:** hold a consequence until the Incident controller has seen its cause ([#837](https://github.com/pkalin/shadewright/issues/837)) ([3e13bdd](https://github.com/pkalin/shadewright/commit/3e13bddd0f167fa55dc789b9ae9b1a2790e599ed))
* **analyzer:** name the container and the restart in the uncertain log instance label ([#967](https://github.com/pkalin/shadewright/issues/967)) ([f6d9586](https://github.com/pkalin/shadewright/commit/f6d95862fe7dfb875bb18c4ae62e216b0c45d5ca))
* **analyzer:** no symptom from a container stopped by its pod's deletion ([#968](https://github.com/pkalin/shadewright/issues/968)) ([fc9ffb4](https://github.com/pkalin/shadewright/commit/fc9ffb45509b006df235655168c819c041003d69))
* **analyzer:** read the newest dead instance of a crash-looping container and never take the kubelet's answer for a log ([#948](https://github.com/pkalin/shadewright/issues/948)) ([8fc924b](https://github.com/pkalin/shadewright/commit/8fc924b6dbf27c72f9d1342b9c811191dadf4dca))
* **analyzer:** redact a YAML block value under a secret key anywhere on a line ([#842](https://github.com/pkalin/shadewright/issues/842)) ([7e5cedb](https://github.com/pkalin/shadewright/commit/7e5cedb15825031a030739a81f4bd7a623596700))
* **analyzer:** redact secret values of more than one token after a key name ([#770](https://github.com/pkalin/shadewright/issues/770)) ([83855cd](https://github.com/pkalin/shadewright/commit/83855cdc66fed66b29fec81b4dc573af9ad73c91))
* **analyzer:** redact six more key-name shapes without hiding a neighbouring key ([#826](https://github.com/pkalin/shadewright/issues/826)) ([ed9ea3d](https://github.com/pkalin/shadewright/commit/ed9ea3d34ab5e154b409ff87f5eef60b29027873)), closes [#769](https://github.com/pkalin/shadewright/issues/769)
* **analyzer:** report a NotRecent skip whose write answer was lost ([#1001](https://github.com/pkalin/shadewright/issues/1001)) ([7af76d5](https://github.com/pkalin/shadewright/commit/7af76d508917983e1688f78fc7e6590c8f86062e))
* **analyzer:** report the deadline when a blob write's context ended before its answer ([#778](https://github.com/pkalin/shadewright/issues/778)) ([ca44cbc](https://github.com/pkalin/shadewright/commit/ca44cbc4256f1fd1062ee02ad9faf5900b324f8f))
* **api:** say what happens to the Incident after AnotherRequestClosed ([#955](https://github.com/pkalin/shadewright/issues/955)) ([49cf4cd](https://github.com/pkalin/shadewright/commit/49cf4cd9ad01520b5d3871fd793af226dc42581b))
* **chart:** let a set podLabels replace the default egress selector ([#986](https://github.com/pkalin/shadewright/issues/986)) ([6a030c0](https://github.com/pkalin/shadewright/commit/6a030c03fea2f3193306ddc76d8b887169a9ca46))
* **chart:** let an Anthropic-only install run without Azure model settings ([#823](https://github.com/pkalin/shadewright/issues/823)) ([84f1824](https://github.com/pkalin/shadewright/commit/84f182423911e73665ebe428bd9c9027b409adf9)), closes [#814](https://github.com/pkalin/shadewright/issues/814)
* end merge waits when the repository is gone or the request is being deleted ([#890](https://github.com/pkalin/shadewright/issues/890)) ([75cd4d5](https://github.com/pkalin/shadewright/commit/75cd4d5f2e7d9ce3b03b25e0cd537fadfdcc6aba))
* **gitops:** abort a handoff at a refused recovery read and watch its replacement for 24 hours ([#838](https://github.com/pkalin/shadewright/issues/838)) ([fe6b89e](https://github.com/pkalin/shadewright/commit/fe6b89edfc64c89636041f479f3c17dd532e987c))
* **gitops:** check delivery and report failures of a stale plan cleanup through a new installation ([#810](https://github.com/pkalin/shadewright/issues/810)) ([7e5e24a](https://github.com/pkalin/shadewright/commit/7e5e24a56a1fadb1ef02b3580364bb8f8c4f5e07)), closes [#797](https://github.com/pkalin/shadewright/issues/797)
* **gitops:** close a stale plan's pull request through a new App installation ([#798](https://github.com/pkalin/shadewright/issues/798)) ([f0b5887](https://github.com/pkalin/shadewright/commit/f0b5887da59f960c07777c09eca4dd96601e31e3)), closes [#290](https://github.com/pkalin/shadewright/issues/290)
* **gitops:** decide on the pull request verdict in one table and conclude gone from every base ([#939](https://github.com/pkalin/shadewright/issues/939)) ([e101fe1](https://github.com/pkalin/shadewright/commit/e101fe1d817f976f92efa448b882e4be65efd65b))
* **gitops:** end a request whose render keeps ending the executor's pod ([#777](https://github.com/pkalin/shadewright/issues/777)) ([cdd6f1d](https://github.com/pkalin/shadewright/commit/cdd6f1d2857ddd09f5c676d603b5127fd530f293)), closes [#714](https://github.com/pkalin/shadewright/issues/714)
* **gitops:** fail the review check of a stale plan ([#766](https://github.com/pkalin/shadewright/issues/766)) ([b518b71](https://github.com/pkalin/shadewright/commit/b518b7174b8c1e5cbdd3a58b9bd7d567061c68b8)), closes [#713](https://github.com/pkalin/shadewright/issues/713)
* **gitops:** keep the target lock while the request's own pull request is open on any base and find it in every state before opening one ([#903](https://github.com/pkalin/shadewright/issues/903)) ([c106497](https://github.com/pkalin/shadewright/commit/c1064970f219684b054cfd7785af482c38037d10))
* **gitops:** name a StatefulSet's old revision only when its status proves it ([#932](https://github.com/pkalin/shadewright/issues/932)) ([1d07201](https://github.com/pkalin/shadewright/commit/1d07201676da974ec0bfb92c62484b092b09ada1))
* **gitops:** record a merge after a target branch change as a stale merge ([#772](https://github.com/pkalin/shadewright/issues/772)) ([aaf9f29](https://github.com/pkalin/shadewright/commit/aaf9f29ddbf9ad9484ab1ea8891cb740a07af692))
* **gitops:** record a merge into the new target branch after a retarget ([#827](https://github.com/pkalin/shadewright/issues/827)) ([5f2632c](https://github.com/pkalin/shadewright/commit/5f2632c250a61bb0d30d7154169085e4478d51d7))
* **gitops:** record every merge of a handed-over pull request on the request that holds the lock ([#1059](https://github.com/pkalin/shadewright/issues/1059)) ([7e99abf](https://github.com/pkalin/shadewright/commit/7e99abf6fd04e87a282a23d5162b8cb3e23006d2)), closes [#870](https://github.com/pkalin/shadewright/issues/870)
* **gitops:** redact each value of the pull request body on its own ([#974](https://github.com/pkalin/shadewright/issues/974)) ([dd3d7e6](https://github.com/pkalin/shadewright/commit/dd3d7e62fa3dc5251ab54648e9f8edb1ea95fb2f))
* **gitops:** report a CustomResourceDefinition that drops status.gitWaits ([#871](https://github.com/pkalin/shadewright/issues/871)) ([87e9697](https://github.com/pkalin/shadewright/commit/87e96974f63ea0fcfe7e3eddaef81038953b248a))
* **gitops:** report a stale close with a pending check and a chain read error correctly ([#818](https://github.com/pkalin/shadewright/issues/818)) ([7c7f3dd](https://github.com/pkalin/shadewright/commit/7c7f3dd45c21d96037e1287670a9aa7677e05db5))
* **gitops:** say why a stale plan cleanup with no pull request recorded retries ([#830](https://github.com/pkalin/shadewright/issues/830)) ([a4fb21a](https://github.com/pkalin/shadewright/commit/a4fb21a7cea8dcbc07744ee2f753ef73b3eb6138))
* **integrations:** check the context's end before classifying a Key Vault signer answer ([#813](https://github.com/pkalin/shadewright/issues/813)) ([adbdef2](https://github.com/pkalin/shadewright/commit/adbdef2be37e6dbc826132458d7c9c4e32c6b056))
* **integrations:** check the context's end before the Key Vault signer classifies an answer ([adbdef2](https://github.com/pkalin/shadewright/commit/adbdef2be37e6dbc826132458d7c9c4e32c6b056)), closes [#781](https://github.com/pkalin/shadewright/issues/781)
* **integrations:** report the context's end before classifying a provider answer ([#782](https://github.com/pkalin/shadewright/issues/782)) ([7559845](https://github.com/pkalin/shadewright/commit/75598454136450d8816bbc3f2759563cb34797e8))
* **integrations:** report the context's end when an answer raced it in azureblob Put ([ca44cbc](https://github.com/pkalin/shadewright/commit/ca44cbc4256f1fd1062ee02ad9faf5900b324f8f)), closes [#767](https://github.com/pkalin/shadewright/issues/767)
* **policy:** refuse a path with ".." anywhere in the path matcher ([#888](https://github.com/pkalin/shadewright/issues/888)) ([b85a2f2](https://github.com/pkalin/shadewright/commit/b85a2f2cac133cf285f49c959a19492dc3d0c52e))
* report the context's end before classifying a provider answer ([7559845](https://github.com/pkalin/shadewright/commit/75598454136450d8816bbc3f2759563cb34797e8)), closes [#779](https://github.com/pkalin/shadewright/issues/779)
* take wait ends from the live request and give every failed read of a merged pull request an end ([#889](https://github.com/pkalin/shadewright/issues/889)) ([e39b9db](https://github.com/pkalin/shadewright/commit/e39b9dbdb107b09b81b9872b04bf0a7ce377e5f0))
* **verifier:** record an executor restart as a reset of the verification window ([#972](https://github.com/pkalin/shadewright/issues/972)) ([c81999b](https://github.com/pkalin/shadewright/commit/c81999bde5da87c2e8d1e2e964b60b9764e5ff1f))

## [0.5.2](https://github.com/pkalin/shadewright/compare/v0.5.1...v0.5.2) (2026-11-15)


### Bug fixes

* **analyzer:** charge a call refused with 429 by Foundry 0 tokens ([#763](https://github.com/pkalin/shadewright/issues/763)) ([b611850](https://github.com/pkalin/shadewright/commit/b61185044f3f7317aa4a816f1effb8e15d7b934f)), closes [#725](https://github.com/pkalin/shadewright/issues/725)
* **analyzer:** drop pod evidence only when its template is retired ([#759](https://github.com/pkalin/shadewright/issues/759)) ([6496eba](https://github.com/pkalin/shadewright/commit/6496ebaaf6178d4654c994e320aace1122babb94))
* **analyzer:** skip probe failures that are part of a pod's start or shutdown ([#764](https://github.com/pkalin/shadewright/issues/764)) ([f00ea10](https://github.com/pkalin/shadewright/commit/f00ea1069283d0eef5f61a31bd819b1bf96633ef)), closes [#726](https://github.com/pkalin/shadewright/issues/726)
* report ready only once the required informers have synced, with startup probes ([#765](https://github.com/pkalin/shadewright/issues/765)) ([739c78e](https://github.com/pkalin/shadewright/commit/739c78ef38ff1232031e0f6edce4b3b2524e2116)), closes [#762](https://github.com/pkalin/shadewright/issues/762)

## [0.5.1](https://github.com/pkalin/shadewright/compare/v0.5.0...v0.5.1) (2026-11-15)


### Performance

* link only client-go and the API types into kubectl-shadewright ([#757](https://github.com/pkalin/shadewright/issues/757)) ([3f9a47e](https://github.com/pkalin/shadewright/commit/3f9a47ec7f6307125aeb26d6db46c29000f3a7e6))

## [0.5.0](https://github.com/pkalin/shadewright/compare/v0.4.0...v0.5.0) (2026-11-15)


### ⚠ BREAKING CHANGES

* a v0.4.x executor image cannot write request status against the v0.5.0 CRDs, so an image-only rollback fails. Roll back the whole chart, CRDs included, which prunes the fields v0.4.x does not know.

### Features

* add the verifier component that writes VerificationReports ([#690](https://github.com/pkalin/shadewright/issues/690)) ([d219022](https://github.com/pkalin/shadewright/commit/d21902249dfa98023913f1e723fa715ed4d59475))
* **analyzer:** add an Anthropic API adapter for the evaluation ([#731](https://github.com/pkalin/shadewright/issues/731)) ([6b9ae9c](https://github.com/pkalin/shadewright/commit/6b9ae9cec0d8857e05ce2ddcfa2d25b4ecd83fa8))
* **analyzer:** choose the model provider per deployment ([#734](https://github.com/pkalin/shadewright/issues/734)) ([3658edb](https://github.com/pkalin/shadewright/commit/3658edb51a2306276393884f2388758859090a68))
* **analyzer:** propose a revert for a request that failed verification ([#704](https://github.com/pkalin/shadewright/issues/704)) ([82538c3](https://github.com/pkalin/shadewright/commit/82538c357ab192a2ff38a36fb2bdb9e343029ddb))
* **api:** add spec.reverts and settle a revert request after the merge ([#701](https://github.com/pkalin/shadewright/issues/701)) ([10e08ab](https://github.com/pkalin/shadewright/commit/10e08ab2481e9efc48a8d1a78a63dc69f23057c7))
* **api:** add the VerificationReport type and the stable window checks ([#686](https://github.com/pkalin/shadewright/issues/686)) ([2fcae2d](https://github.com/pkalin/shadewright/commit/2fcae2d29e9c806c0553c0f2cb5285a788bdd435))
* **api:** record a merge receipt and end a merge without one Inconclusive ([#687](https://github.com/pkalin/shadewright/issues/687)) ([477a35e](https://github.com/pkalin/shadewright/commit/477a35e9555056001f825455cb30beaf48d4d7f0))
* **chart:** mount a model API key file into the analyzer ([#733](https://github.com/pkalin/shadewright/issues/733)) ([0220bc9](https://github.com/pkalin/shadewright/commit/0220bc9b54f5f013649d3ee94e7a5963ef49a399))
* **gitops:** find the candidate manifest of a workload at the applied revision ([#675](https://github.com/pkalin/shadewright/issues/675)) ([62d253b](https://github.com/pkalin/shadewright/commit/62d253b3cc4ae73cab806100d9e19dca390ede5c))
* **gitops:** judge Cleared from the verifier report and end verification ([#694](https://github.com/pkalin/shadewright/issues/694)) ([a8fe47d](https://github.com/pkalin/shadewright/commit/a8fe47d696179317906502494aff31a3d78cdbc5))
* **gitops:** move merged requests through WaitingForDelivery and Verifying ([#691](https://github.com/pkalin/shadewright/issues/691)) ([1b39234](https://github.com/pkalin/shadewright/commit/1b39234be3de8154d194065e30cce3878b397298))
* record ResolvedBeforeRollout from two readings of the Incident count ([#702](https://github.com/pkalin/shadewright/issues/702)) ([ea42557](https://github.com/pkalin/shadewright/commit/ea4255783711635fdfb44559ad8b9115d6072c84))
* report the delivery sources of the watched namespaces in kubectl shadewright doctor ([#689](https://github.com/pkalin/shadewright/issues/689)) ([4bc7141](https://github.com/pkalin/shadewright/commit/4bc7141b995d1675b13a3be9c82fa7f3992655c6))


### Bug fixes

* **analyzer:** redact the value after a secret key name whatever its score ([#707](https://github.com/pkalin/shadewright/issues/707)) ([d6246ba](https://github.com/pkalin/shadewright/commit/d6246ba4a98ae9610d753f9a39a7156affb50168))
* **analyzer:** show the model the ScaledObject and decide its baseline in code ([#716](https://github.com/pkalin/shadewright/issues/716)) ([55d4378](https://github.com/pkalin/shadewright/commit/55d4378fd51c11257b98a884f9cd00fa9570d5eb))
* **gitops:** bound the render check by a child process with OS limits ([#711](https://github.com/pkalin/shadewright/issues/711)) ([7f5c009](https://github.com/pkalin/shadewright/commit/7f5c0098c84a35f7664e8abf76220c59fdfa9130))
* **gitops:** bring the test job back within its time margin ([#718](https://github.com/pkalin/shadewright/issues/718)) ([957d8e0](https://github.com/pkalin/shadewright/commit/957d8e08f4e316eb6e85c1a5677113ad045d64db))
* **gitops:** end a merge conflict in NeedsIntervention with ChangeRequestConflict ([#693](https://github.com/pkalin/shadewright/issues/693)) ([8880304](https://github.com/pkalin/shadewright/commit/8880304076a851811632e7e10a740b1f0c35060e))
* **gitops:** end a queued request when the pull request it waited behind is closed unmerged ([#674](https://github.com/pkalin/shadewright/issues/674)) ([4acd170](https://github.com/pkalin/shadewright/commit/4acd170604c9d69be8b49aa428686574cb12b08f))
* **gitops:** evaluate the review check again when the approval it rested on is gone ([#752](https://github.com/pkalin/shadewright/issues/752)) ([56918a3](https://github.com/pkalin/shadewright/commit/56918a342546bc830a03deab9ffe9986faa6aed2))
* **gitops:** give a refused close an end and keep watching a stale plan ([#712](https://github.com/pkalin/shadewright/issues/712)) ([2bac59a](https://github.com/pkalin/shadewright/commit/2bac59a114f6b078b516e09c8343872f2b24e821))
* **gitops:** keep the target lock until a verified request ends ([#722](https://github.com/pkalin/shadewright/issues/722)) ([cc8b123](https://github.com/pkalin/shadewright/commit/cc8b1232c495cb902ccea867e22be1dd6659bc42))
* **gitops:** look a recorded pull request up by its number on every path ([#697](https://github.com/pkalin/shadewright/issues/697)) ([0e57ce6](https://github.com/pkalin/shadewright/commit/0e57ce681034d29f8ecfa87c3586cc52af674565))
* **gitops:** say in a revert's pull request what it reverts and keep its name readable ([#721](https://github.com/pkalin/shadewright/issues/721)) ([d0ab324](https://github.com/pkalin/shadewright/commit/d0ab32473d33a1f1169d6463bd620755711c0adb))
* **policy:** cap the outcome when a plan that became stale is merged before the periodic check ([#753](https://github.com/pkalin/shadewright/issues/753)) ([f9ee6c5](https://github.com/pkalin/shadewright/commit/f9ee6c5a2eaa0b6ea8314dc3facee9fbd1ebf767))


### Documentation

* mark the image-only rollback from v0.5.0 as a breaking change ([#706](https://github.com/pkalin/shadewright/issues/706)) ([f09d639](https://github.com/pkalin/shadewright/commit/f09d63928bf4d74dabdef0800615c1d4b153f90f)), closes [#705](https://github.com/pkalin/shadewright/issues/705)

## [0.4.0](https://github.com/pkalin/shadewright/compare/v0.3.1...v0.4.0) (2026-11-10)


### Features

* add kubectl shadewright doctor for read-only install checks ([#625](https://github.com/pkalin/shadewright/issues/625)) ([5b7a233](https://github.com/pkalin/shadewright/commit/5b7a23321877e9382009ea883942698ce7fa66da))
* add kubectl shadewright explain for one request or Incident ([#622](https://github.com/pkalin/shadewright/issues/622)) ([fa275e0](https://github.com/pkalin/shadewright/commit/fa275e0175788a6382001c61f888fa87c32a8910))
* add kubectl shadewright status --watch ([#628](https://github.com/pkalin/shadewright/issues/628)) ([7eed19a](https://github.com/pkalin/shadewright/commit/7eed19a5edf38448c5d0e8b615c60d4d16e410e7))
* **analyzer:** back the delivery read tool with the Flux delivery verifier ([#633](https://github.com/pkalin/shadewright/issues/633)) ([c5af4ab](https://github.com/pkalin/shadewright/commit/c5af4ab3a269b565f422bdfec8758f37e49d6703))
* **analyzer:** re-analyze a Proposed Incident with a pending reviewer hint ([#574](https://github.com/pkalin/shadewright/issues/574)) ([743c9f7](https://github.com/pkalin/shadewright/commit/743c9f76eea9fc8274b3d5e83095f88e34aab75e))
* **api:** add reviewer hints and the supersede handoff to the API ([#569](https://github.com/pkalin/shadewright/issues/569)) ([c9008dc](https://github.com/pkalin/shadewright/commit/c9008dc4a4ecc5d35a3a5b62fb053db4ca433f4b)), closes [#535](https://github.com/pkalin/shadewright/issues/535)
* **chart:** add an opt-in flag to turn reviewer hints on ([#635](https://github.com/pkalin/shadewright/issues/635)) ([e89633d](https://github.com/pkalin/shadewright/commit/e89633da6f7f71125eca7cce46b7b665fac825de))
* **chart:** add optional standby replicas for the executor and the analyzer ([#570](https://github.com/pkalin/shadewright/issues/570)) ([12f03e6](https://github.com/pkalin/shadewright/commit/12f03e65dcc00a87986c0e1fe57241109809bc95))
* **gitops:** accept /shadewright reanalyze hints, redact and pin them into the request status ([#582](https://github.com/pkalin/shadewright/issues/582)) ([18d26a0](https://github.com/pkalin/shadewright/commit/18d26a0e7e621cfbabcb31be37408f8fb086d566))
* **gitops:** add the Flux delivery verifier and record the merge commit ([#565](https://github.com/pkalin/shadewright/issues/565)) ([113786b](https://github.com/pkalin/shadewright/commit/113786b695945b042151a0c58120ca1d6aaa61c5)), closes [#92](https://github.com/pkalin/shadewright/issues/92)
* **gitops:** hand a pull request over from a superseded request to its replacement ([#588](https://github.com/pkalin/shadewright/issues/588)) ([9cb6aec](https://github.com/pkalin/shadewright/commit/9cb6aecfc262784a6bb8bbbf6343e452766cb346))
* **gitops:** reply in the pull request with the outcome of a reviewer hint ([#634](https://github.com/pkalin/shadewright/issues/634)) ([c259e24](https://github.com/pkalin/shadewright/commit/c259e246563178afdfc7c1e34df03901685d9304))
* **gitops:** report whether a merged change rolled out ([#573](https://github.com/pkalin/shadewright/issues/573)) ([dd72746](https://github.com/pkalin/shadewright/commit/dd727468bd2f1d8ebb2584ded3b578c290d84a6c))
* **integrations:** read pull request comments, review bodies and the author's permission, bounded ([#572](https://github.com/pkalin/shadewright/issues/572)) ([9c59600](https://github.com/pkalin/shadewright/commit/9c5960086fae269466f2ca31cab263fea9609430))


### Bug fixes

* **analyzer:** ask for the failure's scope and its specific cause ([#637](https://github.com/pkalin/shadewright/issues/637)) ([f91a782](https://github.com/pkalin/shadewright/commit/f91a782c9ac2ef1025f35499ca05cf3c883276f2))
* **analyzer:** keep redaction linear for bare short keys in flow form ([#583](https://github.com/pkalin/shadewright/issues/583)) ([5076c93](https://github.com/pkalin/shadewright/commit/5076c937145b310f402343b1713f9db68db158fe)), closes [#526](https://github.com/pkalin/shadewright/issues/526)
* **analyzer:** redact base64 secrets that slashes cut into short parts ([#584](https://github.com/pkalin/shadewright/issues/584)) ([bad3260](https://github.com/pkalin/shadewright/commit/bad32606330c11b1327ddcac42ce356102c9e407))
* **api:** refuse contradictory handoff states with CEL ([#603](https://github.com/pkalin/shadewright/issues/603)) ([b515547](https://github.com/pkalin/shadewright/commit/b515547df744ff650191fac37887d471330ea5a5))
* **chart:** raise the executor's memory limit to 256Mi ([#617](https://github.com/pkalin/shadewright/issues/617)) ([603189b](https://github.com/pkalin/shadewright/commit/603189b17ae0c29eabcce0d985ad407f419d8f71))
* **chart:** set GOMEMLIMIT below the memory limit of the executor and the analyzer ([#602](https://github.com/pkalin/shadewright/issues/602)) ([daaa263](https://github.com/pkalin/shadewright/commit/daaa263cc838c4f0002058a52666511eab33477e))
* **chart:** set GOMEMLIMIT to 90 percent of the memory limit ([daaa263](https://github.com/pkalin/shadewright/commit/daaa263cc838c4f0002058a52666511eab33477e))
* **gitops:** bound the patcher's YAML node trees ([#605](https://github.com/pkalin/shadewright/issues/605)) ([ffd222a](https://github.com/pkalin/shadewright/commit/ffd222a40fd180b6c7ee1b0498ca508cd7004b64))
* **gitops:** bound what patches and transformers add to the render ([#610](https://github.com/pkalin/shadewright/issues/610)) ([03be1c7](https://github.com/pkalin/shadewright/commit/03be1c705e10519ae30f84653f8b9995df43db16))
* **gitops:** count untargeted patches and bound keys merged into one mapping ([#629](https://github.com/pkalin/shadewright/issues/629)) ([8c194ae](https://github.com/pkalin/shadewright/commit/8c194ae79c2a1d5b7c5d431c35017dc7a5451b99))
* **gitops:** end duplicate requests Superseded when another request's pull request is merged ([#616](https://github.com/pkalin/shadewright/issues/616)) ([a8a5c0c](https://github.com/pkalin/shadewright/commit/a8a5c0ccfcd56a4df463bfc6e19ce251849c3b21))
* **gitops:** recognise the handed-over pull request after a merge commit or refused rewrites ([#607](https://github.com/pkalin/shadewright/issues/607)) ([cd57027](https://github.com/pkalin/shadewright/commit/cd570274de95dbc62f6ef69ae9ff2585c8b26592))
* **gitops:** record the merge commit on the adopt path ([#613](https://github.com/pkalin/shadewright/issues/613)) ([79a91c4](https://github.com/pkalin/shadewright/commit/79a91c4ab061fb710dddcdba1ba8ac7f0b6fc5c9))
* **gitops:** refuse quantities with huge exponents before parsing ([#598](https://github.com/pkalin/shadewright/issues/598)) ([58b4219](https://github.com/pkalin/shadewright/commit/58b4219226522522dc9c2f715feada3fe7119384))
* **gitops:** refuse YAML anchors and aliases in the source mapping ([#599](https://github.com/pkalin/shadewright/issues/599)) ([5de6fe5](https://github.com/pkalin/shadewright/commit/5de6fe5828fc0ee43c46e68293a0557900905335))
* **gitops:** refuse YAML mappings of more than 256 keys or with a duplicate key ([#624](https://github.com/pkalin/shadewright/issues/624)) ([2f22628](https://github.com/pkalin/shadewright/commit/2f22628735e7adc1d49a8758a1fc6d5a46769658))
* **gitops:** surface and retry handoff errors instead of dropping them ([#604](https://github.com/pkalin/shadewright/issues/604)) ([55e78bc](https://github.com/pkalin/shadewright/commit/55e78bce1838d151cf07855184ce2200a31ad673))
* link the AnotherRequestMerged and KustomizationReconciling reasons in explain ([#627](https://github.com/pkalin/shadewright/issues/627)) ([385f465](https://github.com/pkalin/shadewright/commit/385f465a10535539de2b8ae1df2aa29ec078482c))
* redact current-format Entra client secrets whole ([#612](https://github.com/pkalin/shadewright/issues/612)) ([c07bbb3](https://github.com/pkalin/shadewright/commit/c07bbb39770392073a9c72a4b02271f926a8dc92))
* reset condition Approved when the review check runs on a later head ([#611](https://github.com/pkalin/shadewright/issues/611)) ([5914711](https://github.com/pkalin/shadewright/commit/5914711d8f45a473c4827d965173319534d58375))

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
