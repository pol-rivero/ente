import Head from "next/head";
import React from "react";

export const DriveHead: React.FC = () => (
    <Head>
        <title>Ente Drive</title>
        <meta
            name="description"
            content="End-to-end encrypted storage for all your files."
        />
        <link rel="icon" type="image/png" href="/images/favicon.png" />
        <meta name="theme-color" content="#1071FF" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <meta name="referrer" content="strict-origin-when-cross-origin" />
    </Head>
);
