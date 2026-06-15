// The value the new-file Save-path dialog is seeded with, derived from the
// directory the create was launched from (the /new route's ?in= search). From a
// file view the dir is non-empty, so the field prefills `subdir/` with the cursor
// ready to type the new file's name in that same directory; from the project root
// the dir is empty, so the field prefills empty. The result stays fully editable
// to any nested path (B-31 fix #3/#4).
export function newFilePrefill(launchedFrom: string | undefined): string {
  const dir = (launchedFrom ?? '').replace(/\/+$/, '')
  return dir ? `${dir}/` : ''
}
