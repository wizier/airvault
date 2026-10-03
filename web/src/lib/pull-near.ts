/** An attachment calling pull as the element nears the view of its scroller,
 *  its parent; rendered anew per page, it pulls again while it stays in view. */
export function pullNear(pull: () => void, margin = '400px') {
  return (element: HTMLElement) => {
    const io = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) pull();
      },
      { root: element.parentElement, rootMargin: margin },
    );
    io.observe(element);
    return () => io.disconnect();
  };
}
