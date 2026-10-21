# A secret reached memory

Every write path redacts what the secret scanner (`packages/secret-scan`) recognises: passwords given for a label, credentials in URLs, account/password pairs, API keys, JWTs, private keys and similar. A secret the scanner did not recognise when it was written stays in the memory's stored text until it is removed.

## Remove it

1. Find the memories that hold it. `scos_recall` finds the current ones; superseded and forgotten versions still hold the text.
2. Call `scos_redact_history` with each memory's `memory_id`. It rewrites in place, so the history keeps its shape:
   - every version of the memory, in any status;
   - the memory's title, summary and normalised key and value;
   - the diary entries that quote it.

   Every secret the scanner now finds becomes a `[REDACTED:kind]` marker. The reply lists the kinds it removed and how many versions and diary entries it changed. A second call changes nothing.
3. Rotate the secret. Encrypted backups and WAL archives taken before the redaction still hold it until they age out of retention.

## What the database allows

`memory_versions` is append-only. The `memory_versions_append_only` trigger (migration `000062`) admits exactly one kind of update:
- the transaction announces the version through `scos.redacting_version`;
- the update replaces spans of `content` and `structured_content` with redaction markers, adding and changing nothing else;
- every other column stays as it was.

A delete, a rewrite, an addition, or an update that was not announced is refused. History can lose a secret but cannot be forged.

The runtime role may update a version only when it may write candidates to the memory's namespace (policy `000019`).
