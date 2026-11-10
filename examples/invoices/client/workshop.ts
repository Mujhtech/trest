import { Client, InvoiceEventsEvent } from './client';

// Pass a local demo token explicitly; never embed credentials in the generated client.
export async function workshop(baseURL: string, token: string) {
  const client = new Client(baseURL, {
    headers: { Authorization: `Bearer ${token}` },
  });

  const created = await client.createInvoice({
    body: { customer_id: 'customer-acme', amount_minor: 12500, currency: 'NGN' },
  });
  const path = { id: created.data.id };
  const invoice = await client.getInvoice({ path });
  const updated = await client.updateInvoice({
    path,
    body: { amount_minor: 13000, status: 'paid', memo: 'Paid in full' },
  });

  const attached = await client.uploadAttachment({
    path,
    body: { file: new File(['Local workshop receipt'], 'receipt.txt', { type: 'text/plain' }) },
  });
  const list = await client.listInvoices({ query: { currency: 'NGN', limit: 10 } });

  // Breaking iteration cancels the response; this finite feed yields one snapshot.
  const feed = await client.invoiceEvents({ query: { watch: false } });
  let snapshot: InvoiceEventsEvent['snapshot'];
  for await (const event of feed) {
    if (event.event === 'snapshot') snapshot = event.data.snapshot;
  }
  if (!snapshot) throw new Error('The invoice feed did not include a snapshot');
  await client.deleteInvoice({ path });

  return { invoice: invoice.data, updated: updated.data, attachment: attached.data.attachment,
    count: list.data.total, snapshot };
}
