# DontWasteTime (`dwt`)

Local email triage for the Example Microsoft 365 mailboxes: read every enabled
mailbox via Microsoft Graph, classify mail into the cassetti taxonomy, track
threads that need a reply, and email a daily digest plus immediate P0 alerts.
Read-only on the mailboxes: the only mail it sends is the digest and the alerts.

## Quick start

```bash
cp .env.example .env                 # fill MS_* and LLM_* (never commit .env)
cp config.example.json config.json   # set mailboxes, recipients, top clients
make check build
```
