/**
 * Hands a blob to the browser as a file download. The data is already in the
 * page, so nothing is asked of any server here.
 */
export const saveInBrowser = (blob: Blob, name: string): void => {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = name;
  anchor.click();
  // Some browsers start reading the blob after the click returns.
  setTimeout(() => {
    URL.revokeObjectURL(url);
  }, 0);
};
