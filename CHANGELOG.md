# Changelog

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

* production-readiness — session, concurrency, state drift, and acceptance tests ([#33](https://github.com/batonogov/terraform-provider-synology-dsm/issues/33)) ([40ae1ea](https://github.com/batonogov/terraform-provider-synology-dsm/commit/40ae1ea6487fdc57bbd57f5155b27b64c1c351cc))
* sweep leftover acceptance-test resources between runs ([#41](https://github.com/batonogov/terraform-provider-synology-dsm/issues/41)) ([a7570ec](https://github.com/batonogov/terraform-provider-synology-dsm/commit/a7570ec0ba8737a07d18b2fcdb2090a4326a5cc8))
