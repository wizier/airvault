// Attachment factory syncing a <dialog>'s native open state with a boolean:
// use as `{@attach modalOpen(open)}`; it re-runs whenever `open` changes.
export function modalOpen(open: boolean) {
  return (dialog: HTMLDialogElement) => {
    if (open && !dialog.open) dialog.showModal();
    else if (!open && dialog.open) dialog.close();
  };
}
