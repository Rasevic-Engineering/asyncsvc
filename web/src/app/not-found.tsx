import Link from "next/link";

export default function NotFound() {
  return (
    <div className="center">
      <h1>Page not found</h1>
      <Link href="/">Go to your reports</Link>
    </div>
  );
}
