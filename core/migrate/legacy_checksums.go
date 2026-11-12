package migrate

// legacy holds, for every migration up to 0049, the checksum it was recorded with before checksums started to
// skip comment-only lines (todo #264) — raw: sha256 of the whole file, CRLF -> LF — and the comment-free Sum of
// that same original file. Generated from main 106e039, before any comment was edited. Frozen: a migration
// added after 0049 has no entry here.
//
// A row recorded with raw is accepted only while the current file still has the original comment-free sum
// (stripped), so an edit of the SQL is refused on old databases too. Up and Baseline record raw for these
// versions, so an older binary rolled back onto a database migrated by a newer one still finds its own sums.
var legacy = map[int]struct{ raw, stripped string }{
	1:  {raw: "8bda33b4f40a9053c4da0a0e868d4b93411672a6c38395dae78d4af4074371c2", stripped: "5d4573eed6265ad65251c6ee6a3248646e58b65724ec990d592a08123889b5a5"}, // 0001_journal.sql
	2:  {raw: "ee15705c78318ec61a46ea4b2da424bc840811f4dddfaca7bc14f3060b0338fe", stripped: "3bd98f2ed176102f841f61d42f80c31392d3da8d02d4b2e8b2c17b3f86644d0b"}, // 0002_projections.sql
	3:  {raw: "c23207a02885cef27579b7beed76f345ccb9ee92ded804e98cb36a0691d2a2fe", stripped: "7526e3a94e8755671612fe47e8777c98e615335e8c92c126644ed1442fcf34b2"}, // 0003_journal_truncate_guard.sql
	4:  {raw: "9689a44fa929d81b98807301bd896132b73e8ea93f019ed4fd049f5cae80e1cd", stripped: "88878b1197885b7d4bcaf55812cd981ddb64057f1ab2ebe36a0da004dc31151f"}, // 0004_purge_keeps_structure.sql
	5:  {raw: "24a8b8d5d84a9e3fe792baff9f024dc5abab0eae8dd4bbe385971e4d5fcc5bc7", stripped: "5914b9c025c9f28493e53cc9c0459629bee26064656d9dc815261e7cd794123a"}, // 0005_task_v2.sql
	6:  {raw: "ff7b87dbe82e49f627e654886c5b077afbe41c6574937edc6a7d72ecc6b19472", stripped: "a7051ee091f22f37a1d125cec564a87747fd961eb6b0e078f2d455befb6cda5a"}, // 0006_auth.sql
	7:  {raw: "f4d13596164bb25e47ce8c0eb8199c69ae02feb15bbfa1fae6565098f726acfe", stripped: "047f1eac273535266cda39bb7f47fd14c8add46558d734f6ce40e0604c875a65"}, // 0007_facade_ids.sql
	8:  {raw: "1b1047fee0448f330917958527de06feffe147bfa689842bdef1deda059435c9", stripped: "372a33f425219e0475931f7f2b2def6885d571167d5c8cf52e2562c9ccdd8616"}, // 0008_isolation.sql
	9:  {raw: "ce570925ae09238ec60329ce8f59f761195c7af3bf2d1aaaca8b7d623c40cbf4", stripped: "211f932e57411eabf7412cb7dfbf6e5f15c597436f2863c90783c6c8daddab41"}, // 0009_execution_events.sql
	10: {raw: "c84b1c55915a25b681a2957cb6231c786b9987cdfebbe2fa70367fcc1da1dce5", stripped: "f2a53b3c15baf1ffdff1829d64371a9b618e7837d81fbe7e5120e0ec86cdcaf4"}, // 0010_policy.sql
	11: {raw: "109d231eb91a42473e946377d021d85dfcc51796c3ff25fdf20c75eb018bf727", stripped: "d6589f060421ddf42b42db2509db649df999d810f4f38249f7f674c41bf20e89"}, // 0011_token_lifecycle.sql
	12: {raw: "9e7917d84d79a4cfcfb7b4f5f069e420319c611f3f0c3140fa6c68c3afbe3de4", stripped: "9c5198884dd8cbd4340e9b81c638f5db82ccc27182d0573700a0a687d24f024f"}, // 0012_project_links.sql
	13: {raw: "28bc484630bcad6813b03a1a654ff326893edeab8bbf15d54fde61298e456243", stripped: "af427ad5c6e3e5bef6fda85cbb7018d8bc7586059e09806a52a987e6bde57fdb"}, // 0013_oauth.sql
	14: {raw: "68eccea0f3c1aa5c98c0e7d85f2c018a659f26f236b0942b9d42811025515998", stripped: "6883d225fe3303a3dde737ba3a6fd10b9fc77c7abf31649a7fcdd05cd16f8790"}, // 0014_native_api.sql
	15: {raw: "b693a29ce782deb6db8f73d630a6bbf2c6902213576b0bca3f1ce34400f4f062", stripped: "79b2178503de0291dc1b24a4630d3aef70391206fbf9d3111f9019425117e14c"}, // 0015_fact_kinds_layers.sql
	16: {raw: "73fa36fc53acdeb026f5d3afb1936a05b7e2b9ab573c16ec68a17aec2d9c6b80", stripped: "70320b15312025f3e46d4a6d43515faca283d38ee3b0a1e0deffb4732b116c6c"}, // 0016_member_of.sql
	17: {raw: "3dc57816e3e768e154f85305c7e00eff45eaa0f2d10ca204c9acce6925a8bb14", stripped: "9fd75233b325c6ca431b3bfeb7cb104214700e3dc5ec2fa57aa2536ba42d6ed8"}, // 0017_document_type.sql
	18: {raw: "5d5982bcd60a49681a1e8e09fcc1727a6c5d081eea385924eb30fd83914c1568", stripped: "583f8665d05d4507a286227e287186b33b2557a4aec4f21f4497d71cd34c7fa6"}, // 0018_ops_verify.sql
	19: {raw: "28893aa8f458f3990bdcbf8c0e5d42a21ac341a2592675c31fb99b57ac4316f6", stripped: "cd1a685a1f834f433ba3467ac93f2bb4c33fc3275e5a2551baa73c00e14a8fe0"}, // 0019_task_owner_project.sql
	20: {raw: "44d81a08f9a746794914d5773def5da056eed4d98899ba61f1811c4091471910", stripped: "bb867607af2739936650e021f713fdca34a2107de363f62ae4e0273f49dc899e"}, // 0020_workflow.sql
	21: {raw: "43dcc5c8bac51aeb576939e6fcdad972be7f223a83d2cbda59c8ef1109daef6b", stripped: "d50ce8c4f8a6bea94a19e14e6bec8f4efc5056e3085afb2d24796f6747a57126"}, // 0021_execution.sql
	22: {raw: "3df19a32e37aa17733d508c24587e699c900e7d482be6ceb9931b5851f9e8044", stripped: "d1cea78dbe4086038601a2e0db0c2f5bc09a027a1c6f3eff252aae8773d34566"}, // 0022_derived_grants.sql
	23: {raw: "e9108c9b505821a8f7e8ef4ff712724969df6a0b398887e59aae00c529bac195", stripped: "8dda3a6e1cd2103aed385e2cfeda4b51c49891a5e0208470fbef210077d5fc97"}, // 0023_data_class.sql
	24: {raw: "abdf53cd30bd8e3a00271f0473d5e8134356d2e37b8f6473981ce55616053924", stripped: "bd06eecf42bb8e763449c75ea5496bbda94f15b22a118f8e8b8b5eb23de33b01"}, // 0024_executor_profile.sql
	25: {raw: "7b24780a87fb1907719417332085a777b4e2478d601fff52bf5ed50cb84cc23e", stripped: "95be4d21870b02f016067fb88e3859a7347bb2b6245944359d4f5f7ee885b05f"}, // 0025_statement_search.sql
	26: {raw: "273d258cdf3765890c6c7cccf790fbffc9c9acb271ef82b4610a1fdc0e58d148", stripped: "4a78cd48c07b43df84639b925bc24ba00745e9057c2d61e9e9703c42ccc59247"}, // 0026_statement_search_title.sql
	27: {raw: "26f289010ca34c89ecd6dd7261fa0416f884d796cbf60bef1d88a230e5c1bf59", stripped: "fa899947c7028b7a252cbcf2cd896b3148c15b82a878c7a9a9673fb5d186a991"}, // 0027_statement_search_ident_tf.sql
	28: {raw: "24e3a3c5af1e5748894dd376f9388b2446ad99fe81b92bf8c61fc74a8d444e84", stripped: "9788fbfe43783e0fcc8a72abd9802b13d7f9e8f55cc0e9b37c44b63b277457e9"}, // 0028_stem_vocab.sql
	29: {raw: "61a81488c0d710f8bf7a253cc920a5fef50bddadc7cb4dec59580e28b5ae5676", stripped: "b153729d216009dcf439611e3c319ad65affb8376464fb7cf17073b9f1888166"}, // 0029_outcomes.sql
	30: {raw: "d7096235b3972b6ce0fdff14d9fc5246f8493a1b29de7eedc160ea1feef85e53", stripped: "b5d7f89ac6c687374877b83327e77dbac5f8ad8059f29ac2b8b3d67966d72951"}, // 0030_entity_numbers.sql
	31: {raw: "91737eed77cee7a375aa6aed3150f9f67b597c8e402fa1ad1eae2b226f03a8ba", stripped: "d213b7593d217f313b57072ef6343b7f58c3c39d0eea2251b5f959cb2a9a58fc"}, // 0031_refs_mentions.sql
	32: {raw: "f2333a106dd688720df352e7439f117b4b2a0f336ff458c12a128fbf1cae4fcd", stripped: "c540470e6d2f34bf8056a4df3077c7c4cf588040290589c50d3340411e571736"}, // 0032_version_refs.sql
	33: {raw: "6d48d419a3f1315fb996bf2b644cd358401934a0291eb184a298206fc022ac6a", stripped: "a26e4876e4755116d16bfa4686b69888a82b259246a2804247cd6326ec045341"}, // 0033_purge_task.sql
	34: {raw: "e18682390d67f100700bb5b38f7a646aac65f18c18a034cd12594422e89ffd2f", stripped: "2c213303baad2c82a55bbc54d447560b7651b1d09238238f89e66c739e993244"}, // 0034_quarantine.sql
	35: {raw: "e4302c6547eeb3ee87634c50b0acf1c9b52a4e2df14f667b106ca09b5049d9ae", stripped: "45f27bae85149fe513a441a8ad40fcfa2c233a86fb790ab068733b06ca0383e1"}, // 0035_deletion.sql
	36: {raw: "88a1fba93fcd941800e670ad9756f917502e6aa62dda8f7649ea5b6017e52036", stripped: "edcca9d543440d23edb138e8e61a9ccce6f7d800f1124a7af397bcb2d020494e"}, // 0036_retention.sql
	37: {raw: "d67c3de40bb29aec21f504f785ec18dffe02ff011fe663de59e3bac65b6a608d", stripped: "6d5721852c0e951a7d7087211901ffd4b9356917ce0f4cd59ae78e202afe4a4b"}, // 0037_num_floor.sql
	38: {raw: "7bb4a31832c39c9c0beeaab49f66f8a2614e5f26de15da083815bcf8ccffa9e1", stripped: "138545ded1dcea193a54d80a1dfe144c485a174360b20151788c616450e37eab"}, // 0038_family.sql
	39: {raw: "d5153bdffab50be6c7c2ea0b8dafc9f63b1ba457fadffb518670003d8847ecf8", stripped: "bfc6e9d13f71b3c35c2be1476805d45b5c84da0460d03e988b402b7c813b010c"}, // 0039_fact_confirmed.sql
	40: {raw: "2fc22dc65a85dac8fd2383bc448c583f56f8e7b09779b5eb929ac1ec8270a104", stripped: "f87ace15dcd5454c67dfd400ec9ca18ec842767ee183bc339a5118d2ba50deb1"}, // 0040_legacy_type.sql
	41: {raw: "4eb1ea9453ba58e154e3060d8b49438ef894ea21f9494bc4faeb73973e01947d", stripped: "562aa0a5263a737ac687bb8c14c936e9e3949d5f4f13a5cd3d151d4a76ae8e0f"}, // 0041_project_scope.sql
	42: {raw: "c195fc8502b943f55181a715c69b17d55534ad5e997a30db8add705d10079a62", stripped: "542ef6431690aa89d4c77f3b765c8795e13fe24ad32eef9abcdf7924b05b8176"}, // 0042_maintenance.sql
	43: {raw: "25b7efeb29f34e70e2261255ed22d53c7252febf59090c4dc2930a1f2c6a71e6", stripped: "edcda6750e6da084ad94b8b8e618d1568406cc3b17fc08ab3122fb6339bf77ff"}, // 0043_record_retention.sql
	44: {raw: "fe334f7aecc7e2d80a298dad3db9bd3dad56a7a458618b22f56b2e31fa2c6440", stripped: "43664f07645726564920c24648e52629edf8521db5dbd6205455b76756e28596"}, // 0044_link_resolves.sql
	45: {raw: "465b45437ddb311b9162e14380edbab8d5661537488ea66ec9820997622a7dc9", stripped: "e0bbc15da213511745b306200192c4999577095e50561ac208679d4e5b89712c"}, // 0045_executor_keys.sql
	46: {raw: "e9a17ffe26c9c062439b26fac6b6eda37c656db6bb4defa8b391833635b4571c", stripped: "92181583dd1393ca530c6e5d099241ef79448a412fea2b8278b3483ecc7be2ee"}, // 0046_statement_fields.sql
	47: {raw: "fc8a338591b1e114aa6ddf7c1510e2788961c1207bdef88c1c32a2beb9e1126f", stripped: "0e691c189edcf6a3f37b74a5b59f39ab692ae2c7b8f979faa349761884504def"}, // 0047_fact_source.sql
	48: {raw: "ea7d64882fbb3bfd19fda8830044b1db1c795c316efb88ac541062d92958f302", stripped: "c10e5611b794924ddd598331a29188d7b4f30c89d2bebdd96d80d84a0d1ab40e"}, // 0048_embeddings.sql
	49: {raw: "7bb7d5fac49313505ca59a2dd2ba73139954ef50f2a6867d2d34d46f7d524b0e", stripped: "22b238ab83eea2cfabc00bcb3b585a063412dae3b78d66a12df633d925706fb5"}, // 0049_attachments.sql
}

// LegacySum is the whole-file checksum a migration was recorded with before the switch, "" after 0049.
func LegacySum(version int) string { return legacy[version].raw }
