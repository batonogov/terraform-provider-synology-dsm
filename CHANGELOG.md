# Changelog

## [0.2.0](https://github.com/batonogov/terraform-provider-synology-dsm/compare/v0.1.1...v0.2.0) (2026-08-12)


### Features

* add dsm_file resource for uploading files into shared folders ([#66](https://github.com/batonogov/terraform-provider-synology-dsm/issues/66)) ([e6b467b](https://github.com/batonogov/terraform-provider-synology-dsm/commit/e6b467b481df0b0048581f6f31c040ba25d2856e))
* allow adopting an existing shared folder ([#60](https://github.com/batonogov/terraform-provider-synology-dsm/issues/60)) ([1a2a2c5](https://github.com/batonogov/terraform-provider-synology-dsm/commit/1a2a2c5764a9b23064db0fa1f5f42d258e70497a))
* manage DSM firewall rules ([#76](https://github.com/batonogov/terraform-provider-synology-dsm/issues/76)) ([2fdf5a0](https://github.com/batonogov/terraform-provider-synology-dsm/commit/2fdf5a0e5b8ea9120d0d29feba3ef907f05f1d0c)), closes [#61](https://github.com/batonogov/terraform-provider-synology-dsm/issues/61)
* manage DSM reverse proxy entries ([#74](https://github.com/batonogov/terraform-provider-synology-dsm/issues/74)) ([7dff3d0](https://github.com/batonogov/terraform-provider-synology-dsm/commit/7dff3d09fad682ab1febd05a33c399f24e8d83cb))
* manage DSM Task Scheduler and event tasks ([#77](https://github.com/batonogov/terraform-provider-synology-dsm/issues/77)) ([0694b3a](https://github.com/batonogov/terraform-provider-synology-dsm/commit/0694b3ac1dac2ff37ea32e673f39a09edf847766))
* manage DSM time zone and NTP settings ([#68](https://github.com/batonogov/terraform-provider-synology-dsm/issues/68)) ([1405e85](https://github.com/batonogov/terraform-provider-synology-dsm/commit/1405e857a32144399645083f23abb10b9f4013c1))
* manage TLS certificates (import and Let's Encrypt) ([#78](https://github.com/batonogov/terraform-provider-synology-dsm/issues/78)) ([766ce84](https://github.com/batonogov/terraform-provider-synology-dsm/commit/766ce84562d06e77094925ea0cd9c944861321ff))


### Bug Fixes

* catch over-long share descriptions and stop overselling 3300 ([#71](https://github.com/batonogov/terraform-provider-synology-dsm/issues/71)) ([dd73747](https://github.com/batonogov/terraform-provider-synology-dsm/commit/dd73747e8d02a99fc3c3c891c8042a5d5f0f996f)), closes [#65](https://github.com/batonogov/terraform-provider-synology-dsm/issues/65)
* do not lose a project to a late DSM response ([#75](https://github.com/batonogov/terraform-provider-synology-dsm/issues/75)) ([12270a6](https://github.com/batonogov/terraform-provider-synology-dsm/commit/12270a65fb54166151c7d51c8e4ff38032378bec)), closes [#70](https://github.com/batonogov/terraform-provider-synology-dsm/issues/70)
* surface Container Manager's own reason for a failed build ([#72](https://github.com/batonogov/terraform-provider-synology-dsm/issues/72)) ([09b7ee8](https://github.com/batonogov/terraform-provider-synology-dsm/commit/09b7ee8d35a058ed4c45773f78e138b0f115cc60)), closes [#67](https://github.com/batonogov/terraform-provider-synology-dsm/issues/67)
* treat a partially running project as running ([#73](https://github.com/batonogov/terraform-provider-synology-dsm/issues/73)) ([72eb856](https://github.com/batonogov/terraform-provider-synology-dsm/commit/72eb85615e739d474d660145c2676ff586c4437d)), closes [#69](https://github.com/batonogov/terraform-provider-synology-dsm/issues/69)

## [0.1.1](https://github.com/batonogov/terraform-provider-synology-dsm/compare/v0.1.0...v0.1.1) (2026-08-12)


### Bug Fixes

* report DSM errors as sentences instead of bare codes ([#54](https://github.com/batonogov/terraform-provider-synology-dsm/issues/54)) ([e808aa4](https://github.com/batonogov/terraform-provider-synology-dsm/commit/e808aa4d563f93c9f1210b73f41900b352467d60))
* serialise share mutations and retry DSM busy codes ([#56](https://github.com/batonogov/terraform-provider-synology-dsm/issues/56)) ([ef1ad32](https://github.com/batonogov/terraform-provider-synology-dsm/commit/ef1ad325d7251a8aedd08c19161773cfed7ee5e3))

## 0.1.0 (2026-08-12)


### Features

* add acceptance tests with virtual-dsm test environment ([#23](https://github.com/batonogov/terraform-provider-synology-dsm/issues/23)) ([79ca702](https://github.com/batonogov/terraform-provider-synology-dsm/commit/79ca7023b1893f33f48627c044d91136e92687a3))
* add data source dsm_user ([#19](https://github.com/batonogov/terraform-provider-synology-dsm/issues/19)) ([ab47e51](https://github.com/batonogov/terraform-provider-synology-dsm/commit/ab47e51113ff85028fecfeb472cb8a61e165a5ef))
* add DSM setup script and acceptance tests ([#27](https://github.com/batonogov/terraform-provider-synology-dsm/issues/27)) ([d275628](https://github.com/batonogov/terraform-provider-synology-dsm/commit/d27562819ec75f4f89908af52b721dad09862521))
* add dsm_user expiry and 2FA status, fix update and the disabled flag ([#42](https://github.com/batonogov/terraform-provider-synology-dsm/issues/42)) ([202d168](https://github.com/batonogov/terraform-provider-synology-dsm/commit/202d16831863204fdcd1de1a21d7efbe8324e0cd))
* add resource and data source dsm_group ([#20](https://github.com/batonogov/terraform-provider-synology-dsm/issues/20)) ([e2f2784](https://github.com/batonogov/terraform-provider-synology-dsm/commit/e2f2784e79960bee69d70481a5e26890c3aafb1a))
* add resource and data source dsm_share_permission ([#25](https://github.com/batonogov/terraform-provider-synology-dsm/issues/25)) ([26eb57f](https://github.com/batonogov/terraform-provider-synology-dsm/commit/26eb57fee5138c6b3e1c2e42c978a17f519ebcc3))
* add resource and data source dsm_shared_folder ([#21](https://github.com/batonogov/terraform-provider-synology-dsm/issues/21)) ([0ad6287](https://github.com/batonogov/terraform-provider-synology-dsm/commit/0ad62873b4e230e79d99ac4c9a6a20ff8b574905))
* add resource and data source dsm_user_home_service ([#39](https://github.com/batonogov/terraform-provider-synology-dsm/issues/39)) ([3a28dd4](https://github.com/batonogov/terraform-provider-synology-dsm/commit/3a28dd4dc73a0d4cc5e3f092351cffba4f923512))
* add resource and data source dsm_user_quota ([#26](https://github.com/batonogov/terraform-provider-synology-dsm/issues/26)) ([a956602](https://github.com/batonogov/terraform-provider-synology-dsm/commit/a956602c3db05d9c7a47c8985644bdbfdef7d678))
* extend dsm_shared_folder attributes and fix the broken update path ([#40](https://github.com/batonogov/terraform-provider-synology-dsm/issues/40)) ([b8106e7](https://github.com/batonogov/terraform-provider-synology-dsm/commit/b8106e747a179048d13c76b80dc8b2043a742d7a))
* initial Terraform provider for Synology DSM ([#1](https://github.com/batonogov/terraform-provider-synology-dsm/issues/1)) ([a74b4d9](https://github.com/batonogov/terraform-provider-synology-dsm/commit/a74b4d9bdbe51bbf37ac138aff091d2da951d837))
* manage Container Manager projects ([#44](https://github.com/batonogov/terraform-provider-synology-dsm/issues/44)) ([88206ee](https://github.com/batonogov/terraform-provider-synology-dsm/commit/88206eef8f09e4fcaecca02817196d7dd8c3ee9b))
* manage DSM packages ([#43](https://github.com/batonogov/terraform-provider-synology-dsm/issues/43)) ([6395af7](https://github.com/batonogov/terraform-provider-synology-dsm/commit/6395af76e047a7be16d5dca49f30460f9891103c))


### Bug Fixes

* correct initial Release Please manifest ([#47](https://github.com/batonogov/terraform-provider-synology-dsm/issues/47)) ([748677a](https://github.com/batonogov/terraform-provider-synology-dsm/commit/748677a64cbfb4088e63004c41d994846128e79a))
* production-readiness — session, concurrency, state drift, and acceptance tests ([#33](https://github.com/batonogov/terraform-provider-synology-dsm/issues/33)) ([40ae1ea](https://github.com/batonogov/terraform-provider-synology-dsm/commit/40ae1ea6487fdc57bbd57f5155b27b64c1c351cc))
* sweep leftover acceptance-test resources between runs ([#41](https://github.com/batonogov/terraform-provider-synology-dsm/issues/41)) ([a7570ec](https://github.com/batonogov/terraform-provider-synology-dsm/commit/a7570ec0ba8737a07d18b2fcdb2090a4326a5cc8))
