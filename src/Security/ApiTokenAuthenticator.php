<?php
declare(strict_types=1);

namespace App\Security;

use App\Api\V1\DTO\ApiResponse;
use App\Repository\ApiTokenRepository;
use Symfony\Component\HttpFoundation\{JsonResponse, Request, Response};
use Symfony\Component\Security\Core\Authentication\Token\TokenInterface;
use Symfony\Component\Security\Core\Exception\{AuthenticationException, CustomUserMessageAuthenticationException};
use Symfony\Component\Security\Http\Authenticator\AbstractAuthenticator;
use Symfony\Component\Security\Http\Authenticator\Passport\{Badge\UserBadge, Passport, SelfValidatingPassport};
use Symfony\Component\Serializer\SerializerInterface;

class ApiTokenAuthenticator extends AbstractAuthenticator
{
    public const TOKEN_HEADER = 'api-token';

    public function __construct(
        private readonly SerializerInterface $serializer,
        private readonly ApiTokenRepository $tokenRepo,
    ) {

    }

    /**
     * Called on every request to decide if this authenticator should be
     * used for the request. Returning `false` will cause this authenticator
     * to be skipped.
     *
     * @see https://symfony.com/doc/6.1/security/custom_authenticator.html
     */
    public function supports(Request $request): bool
    {
        return static::requestHasToken($request);
    }

    public function authenticate(Request $request): Passport
    {
        $tokenKey = static::getTokenKeyFromRequest($request);

        if (null === $tokenKey) {
            throw new CustomUserMessageAuthenticationException('No API token provided');
        }

        return new SelfValidatingPassport(
            new UserBadge($tokenKey, function (string $userIdentifier) {
                return $this->tokenRepo->findUserByTokenKey($userIdentifier);
            })
        );
    }

    public function onAuthenticationSuccess(Request $request, TokenInterface $token, string $firewallName): ?Response
    {
        // No response object needed in token auth
        return null;
    }

    public function onAuthenticationFailure(Request $request, AuthenticationException $exception): JsonResponse
    {
        // @todo Decouple with App\Api\V1\DTO
        $json = $this->serializer->serialize(
            new ApiResponse(null, JsonResponse::HTTP_UNAUTHORIZED, $exception->getMessageKey()),
            'json',
            ['groups' => ['api']]
        );

        return new JsonResponse($json, JsonResponse::HTTP_UNAUTHORIZED, json: true);
    }

    public static function getTokenKeyFromRequest(Request $request): ?string
    {
        return $request?->headers?->get(self::TOKEN_HEADER) ?:
            $request?->cookies?->get(self::TOKEN_HEADER) ?:
                $request?->query?->get(self::TOKEN_HEADER);
    }

    public static function requestHasToken(Request $request): bool
    {
        // Let's also support cookies and query params for some cases like torrent clients.
        return $request->headers->has(self::TOKEN_HEADER) ||
            $request->cookies->has(self::TOKEN_HEADER) ||
            $request->query->has(self::TOKEN_HEADER);
    }
}
