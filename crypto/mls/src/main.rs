//! Private pipe protocol. Never expose this process as a remote MCP tool.
//! OpenMLS owns all group key schedules and deletes consumed ratchet secrets.
use anyhow::{Result, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD as B64};
use openmls::prelude::tls_codec::{Deserialize as TlsDeserialize, Serialize as TlsSerialize};
use openmls::prelude::*;
use openmls_basic_credential::SignatureKeyPair;
use openmls_rust_crypto::OpenMlsRustCrypto;
use openmls_traits::OpenMlsProvider;
use serde::{Deserialize, Serialize};
use std::io::{Read, Write};

const SUITE: Ciphersuite = Ciphersuite::MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519;
const MAX_INPUT: u64 = 64 * 1024 * 1024;

#[derive(Debug)]
struct RejectedApplication;
impl std::fmt::Display for RejectedApplication {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("invalid MLS application message")
    }
}
impl std::error::Error for RejectedApplication {}

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
struct Request {
    op: String,
    state: Vec<(String, String)>,
    signing_key: String,
    group: String,
    data: String,
    aad: String,
    member: String,
}
#[derive(Default, Serialize)]
struct Response {
    state: Vec<(String, String)>,
    data: String,
    welcome: String,
    sender: String,
    aad: String,
    epoch: u64,
    members: Vec<String>,
}
fn join_config() -> MlsGroupJoinConfig {
    MlsGroupJoinConfig::builder()
        .use_ratchet_tree_extension(true)
        .max_past_epochs(0)
        .sender_ratchet_configuration(SenderRatchetConfiguration::new(128, 4096))
        .build()
}
fn run(req: Request) -> Result<Response> {
    let provider = OpenMlsRustCrypto::default();
    for (k, v) in req.state {
        provider
            .storage()
            .values
            .write()
            .unwrap()
            .insert(B64.decode(k)?, B64.decode(v)?);
    }
    let private = B64.decode(&req.signing_key)?;
    ensure!(private.len() == 64, "invalid signing key");
    let signer = SignatureKeyPair::from_raw(
        SignatureScheme::ED25519,
        private[..32].to_vec(),
        private[32..].to_vec(),
    );
    let credential = CredentialWithKey {
        credential: BasicCredential::new(private[32..].to_vec()).into(),
        signature_key: private[32..].to_vec().into(),
    };
    let group_id = GroupId::from_slice(req.group.as_bytes());
    let mut out = Response::default();
    if req.op == "key_package" {
        let kp = KeyPackage::builder().build(SUITE, &provider, &signer, credential)?;
        out.data = B64.encode(kp.key_package().tls_serialize_detached()?);
    } else {
        let mut group = match req.op.as_str() {
            "create" => {
                ensure!(
                    MlsGroup::load(provider.storage(), &group_id)?.is_none(),
                    "group already exists"
                );
                let config = MlsGroupCreateConfig::builder()
                    .ciphersuite(SUITE)
                    .use_ratchet_tree_extension(true)
                    .max_past_epochs(0)
                    .sender_ratchet_configuration(SenderRatchetConfiguration::new(128, 4096))
                    .build();
                MlsGroup::new_with_group_id(
                    &provider,
                    &signer,
                    &config,
                    group_id.clone(),
                    credential,
                )?
            }
            "join" => {
                ensure!(
                    MlsGroup::load(provider.storage(), &group_id)?.is_none(),
                    "group already exists"
                );
                let msg = MlsMessageIn::tls_deserialize_exact(B64.decode(&req.data)?)?;
                let MlsMessageBodyIn::Welcome(welcome) = msg.extract() else {
                    bail!("expected welcome")
                };
                StagedWelcome::new_from_welcome(&provider, &join_config(), welcome, None)?
                    .into_group(&provider)?
            }
            _ => MlsGroup::load(provider.storage(), &group_id)?
                .ok_or_else(|| anyhow::anyhow!("missing group"))?,
        };
        ensure!(
            group.group_id() == &group_id && group.ciphersuite() == SUITE,
            "unexpected group context"
        );
        match req.op.as_str() {
            "create" | "join" | "info" => {}
            "add" => {
                let kp = KeyPackageIn::tls_deserialize_exact(B64.decode(&req.data)?)?
                    .validate(provider.crypto(), ProtocolVersion::Mls10)?;
                ensure!(
                    B64.encode(kp.leaf_node().signature_key().as_slice()) == req.member,
                    "unverified device key"
                );
                ensure!(
                    kp.leaf_node().credential().serialized_content()
                        == kp.leaf_node().signature_key().as_slice(),
                    "unverified credential"
                );
                let (commit, welcome, _) = group.add_members(&provider, &signer, &[kp])?;
                out.data = B64.encode(commit.tls_serialize_detached()?);
                out.welcome = B64.encode(welcome.tls_serialize_detached()?);
            }
            "remove" => {
                let index = group
                    .members()
                    .find(|m| B64.encode(&m.signature_key) == req.member)
                    .ok_or_else(|| anyhow::anyhow!("unknown member"))?
                    .index;
                let (commit, _, _) = group.remove_members(&provider, &signer, &[index])?;
                out.data = B64.encode(commit.tls_serialize_detached()?);
            }
            "update" => {
                let bundle =
                    group.self_update(&provider, &signer, LeafNodeParameters::default())?;
                out.data = B64.encode(bundle.commit().tls_serialize_detached()?);
            }
            "finalize" => group.merge_pending_commit(&provider)?,
            "discard" => group.clear_pending_commit(provider.storage())?,
            "encrypt" => {
                group.set_aad(B64.decode(&req.aad)?);
                let message = group.create_message(&provider, &signer, &B64.decode(&req.data)?)?;
                out.data = B64.encode(message.tls_serialize_detached()?);
            }
            "decrypt" | "commit" => {
                // Only message processing failures are recoverable rejections.
                // Loading state, creating the provider, and other operations
                // remain fatal; callers must never skip on infrastructure errors.
                let processed = (|| -> Result<_> {
                    let msg = MlsMessageIn::tls_deserialize_exact(B64.decode(&req.data)?)?
                        .try_into_protocol_message()?;
                    let processed = group.process_message(&provider, msg)?;
                    if req.op == "decrypt" {
                        ensure!(
                            matches!(
                                processed.content(),
                                ProcessedMessageContent::ApplicationMessage(_)
                            ),
                            "expected application message"
                        );
                    }
                    Ok(processed)
                })()
                .map_err(|err| {
                    if req.op == "decrypt" {
                        anyhow::Error::new(RejectedApplication)
                    } else {
                        err
                    }
                })?;
                out.sender = B64.encode(processed.credential().serialized_content());
                out.aad = B64.encode(processed.aad());
                match processed.into_content() {
                    ProcessedMessageContent::ApplicationMessage(data) if req.op == "decrypt" => {
                        out.data = B64.encode(data.into_bytes())
                    }
                    ProcessedMessageContent::StagedCommitMessage(commit) if req.op == "commit" => {
                        group.merge_staged_commit(&provider, *commit)?
                    }
                    _ => bail!("unexpected MLS message type"),
                }
            }
            _ => bail!("unknown operation"),
        }
        out.epoch = group.epoch().as_u64();
        for member in group.members() {
            ensure!(
                member.credential.serialized_content() == member.signature_key,
                "invalid member credential"
            );
            out.members.push(B64.encode(member.signature_key));
        }
        out.members.sort();
    }
    out.state = provider
        .storage()
        .values
        .read()
        .unwrap()
        .iter()
        .map(|(k, v)| (B64.encode(k), B64.encode(v)))
        .collect();
    out.state.sort();
    Ok(out)
}
fn main() {
    let result = (|| -> Result<Response> {
        let mut input = Vec::new();
        std::io::stdin()
            .take(MAX_INPUT + 1)
            .read_to_end(&mut input)?;
        ensure!(input.len() as u64 <= MAX_INPUT, "request too large");
        run(serde_json::from_slice(&input)?)
    })();
    match result {
        Ok(response) => {
            serde_json::to_writer(std::io::stdout().lock(), &response)
                .expect("private pipe closed");
        }
        Err(err) => {
            // Do not print library errors: they may contain application content or secrets.
            let _ = std::io::stderr().write_all(b"MLS operation rejected\n");
            std::process::exit(if err.is::<RejectedApplication>() {
                2
            } else {
                1
            });
        }
    }
}
