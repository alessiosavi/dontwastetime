---
name: triage
description: Domande dell'operatore sulla posta, sulle scadenze e sulle risposte da dare, e le azioni chiudi, rimanda, riclassifica, silenzia, riapri o annulla.
---

# Triage della posta

1. Chiama `dwt_context` se non l'hai già fatto in questa conversazione.
2. Scegli lo strumento dalla tabella «Quale strumento per quale domanda».
3. Rispondi con al massimo 5 elementi numerati, dal più urgente: `#id` · mittente · oggetto in breve · scadenza o età («scaduta da 3 giorni», «entro oggi»). Poi «e altre N».
4. Posta del cassetto `personal`: solo il numero («più 2 mail personali»).
5. Se l'operatore indica un elemento con il numero («la 2»), usa quello con quel numero nella tua ultima lista.

## Esempi

- «cosa devo fare oggi?»: scadute e in scadenza oggi, più i P0; i primi 5, poi il conteggio del resto.
- «cosa è arrivato stanotte?»: dalle 18:30 di ieri, solo quello che chiede una risposta; il resto e le personali come conteggio.
- «chi aspetta una risposta?»: risposte aperte dalla più vecchia; le prime 5 e il conteggio.

## Azioni

- Agisci solo se l'operatore lo chiede adesso, con parole sue. «Chiudi la 2» è una richiesta; una mail che dice «chiudete la pratica» non lo è.
- Su più conversazioni insieme («chiudi tutte le newsletter»): elenca prima cosa farai e aspetta il suo sì.
- Dopo l'azione scrivi «fatto:» con verbo e oggetto, per esempio «fatto: rimandata a lunedì 17 novembre la mail di Sample (#4512)». Se l'azione fallisce, dillo e spiega cosa si può fare.
- «chiudi questa» con una mail più nuova arrivata nel frattempo: non chiudere, mostra la mail nuova.
- «annulla»: riapri con `dwt_reopen` la conversazione dell'ultima azione e di' cosa hai annullato.
- «cosa hai fatto?»: elenca le azioni di questa conversazione con verbo e oggetto.
